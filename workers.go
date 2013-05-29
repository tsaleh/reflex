package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/howeyc/fsnotify"
)

var (
	seqCommands = &sync.Mutex{}
)

type OutMsg struct {
	reflexID int
	message  string
}

const (
	// ANSI colors -- using 32 - 36
	colorStart = 32
	numColors  = 5
)

func infoPrintln(id int, args ...interface{}) { stdout <- OutMsg{id, fmt.Sprint(args...)} }
func infoPrintf(id int, format string, args ...interface{}) {
	stdout <- OutMsg{id, fmt.Sprintf(format, args...)}
}

func walker(watcher *fsnotify.Watcher) filepath.WalkFunc {
	return func(path string, f os.FileInfo, err error) error {
		if err != nil || !f.IsDir() {
			// TODO: Is there some other thing we should be doing to handle errors? When watching large
			// directories that have lots of programs modifying them (esp. if they're making tempfiles along the
			// way), we often get errors.
			return nil
		}
		if verbose {
			infoPrintln(-1, "Adding watch for path", path)
		}
		if err := watcher.Watch(path); err != nil {
			// TODO: handle this somehow?
			infoPrintf(-1, "Error while watching new path %s: %s\n", path, err)
		}
		return nil
	}
}

func broadcast(in <-chan string, outs []chan<- string) {
	for e := range in {
		for _, out := range outs {
			out <- e
		}
	}
}

func watch(root string, watcher *fsnotify.Watcher, names chan<- string, done chan<- error) {
	if err := watcher.Watch(root); err != nil {
		Fatalln(err)
	}

	for {
		select {
		case e := <-watcher.Event:
			path := strings.TrimPrefix(e.Name, "./")
			names <- path
			if e.IsCreate() {
				if err := filepath.Walk(path, walker(watcher)); err != nil {
					// TODO: handle this somehow?
					infoPrintf(-1, "Error while walking path %s: %s\n", path, err)
				}
			}
			if e.IsDelete() {
				watcher.RemoveWatch(path)
			}
		case err := <-watcher.Error:
			done <- err
			return
		}
	}
}

// filterMatching passes on messages matching the regex/glob.
func filterMatching(in <-chan string, out chan<- string, reflex *Reflex) {
	for name := range in {
		if reflex.useRegex {
			if !reflex.regex.MatchString(name) {
				continue
			}
		} else {
			matches, err := filepath.Match(reflex.glob, name)
			// TODO: It would be good to notify the user on an error here.
			if err != nil {
				infoPrintln(reflex.id, "Error matching glob:", err)
				continue
			}
			if !matches {
				continue
			}
		}
		// TODO: These only match if the file/dir still exists...not sure if there's a better way.
		if reflex.onlyFiles || reflex.onlyDirs {
			stat, err := os.Stat(name)
			if err != nil {
				continue
			}
			if (reflex.onlyFiles && stat.IsDir()) || (reflex.onlyDirs && !stat.IsDir()) {
				continue
			}
		}
		out <- name
	}
}

// batch receives realtime file notification events and batches them up. It's a bit tricky, but here's what
// it accomplishes:
// * When we initially get a message, wait a bit and batch messages before trying to send anything. This is
//	 because the file events come in quick bursts.
// * Once it's time to send, don't do it until the out channel is unblocked. In the meantime, keep batching.
//   When we've sent off all the batched messages, go back to the beginning.
func batch(in <-chan string, out chan<- string, reflex *Reflex) {
	for name := range in {
		reflex.backlog.Add(name)
		timer := time.NewTimer(200 * time.Millisecond)
	outer:
		for {
			select {
			case name := <-in:
				reflex.backlog.Add(name)
			case <-timer.C:
				for {
					select {
					case name := <-in:
						reflex.backlog.Add(name)
					case out <- reflex.backlog.Next():
						if reflex.backlog.RemoveOne() {
							break outer
						}
					}
				}
			}
		}
	}
}

// runEach runs the command on each name that comes through the names channel. Each {} is replaced by the name
// of the file. The stderr and stdout of the command are passed line-by-line to the stderr and stdout chans.
// TODO: runEach/runCommand could really use a nice cleanup.
func runEach(names <-chan string, reflex *Reflex) {
outer:
	for name := range names {
		if reflex.startService {
			select {
			case <-reflex.done:
				infoPrintln(reflex.id, "Restarting service")
				runCommand(reflex, name, stdout, stderr)
				continue outer
			default:
				if err := reflex.cmd.Process.Signal(os.Interrupt); err != nil {
					infoPrintln(reflex.id, "Error sending interrupt:", err)
				}
			}
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-reflex.done:
					infoPrintln(reflex.id, "Restarting service")
					runCommand(reflex, name, stdout, stderr)
					continue outer
				case <-ticker.C:
					if err := reflex.cmd.Process.Kill(); err != nil {
						infoPrintln(reflex.id, "Error killing process:", err)
					}
				}
			}
		} else {
			runCommand(reflex, name, stdout, stderr)
			<-reflex.done
		}
	}
}

func replaceSubSymbol(command []string, subSymbol string, name string) []string {
	replacer := strings.NewReplacer(subSymbol, name)
	newCommand := make([]string, len(command))
	for i, c := range command {
		newCommand[i] = replacer.Replace(c)
	}
	return newCommand
}

// runCommand runs the given Command. All output is passed line-by-line to the stderr/stdout channels.
func runCommand(reflex *Reflex, name string, stdout chan<- OutMsg, stderr chan<- OutMsg) {
	command := replaceSubSymbol(reflex.command, reflex.subSymbol, name)
	cmd := exec.Command(command[0], command[1:]...)
	reflex.cmd = cmd

	if flagSequential {
		seqCommands.Lock()
	}

	cmdout, err := cmd.StdoutPipe()
	if err != nil {
		infoPrintln(reflex.id, err)
		return
	}
	cmderr, err := cmd.StderrPipe()
	if err != nil {
		infoPrintln(reflex.id, err)
		return
	}

	if err := cmd.Start(); err != nil {
		infoPrintln(reflex.id, err)
		return
	}

	stdoutErr := make(chan error)
	go func() {
		scanner := bufio.NewScanner(cmdout)
		for scanner.Scan() {
			stdout <- OutMsg{reflex.id, scanner.Text()}
		}
		stdoutErr <- scanner.Err()
	}()

	stderrErr := make(chan error)
	go func() {
		scanner := bufio.NewScanner(cmderr)
		for scanner.Scan() {
			stderr <- OutMsg{reflex.id, scanner.Text()}
		}
		stderrErr <- scanner.Err()
	}()

	cmdErr := make(chan error)
	go func() {
		cmdErr <- cmd.Wait()
	}()

	done := make(chan struct{})
	reflex.done = done
	go func() {
		for {
			select {
			case err := <-stdoutErr:
				if err != nil {
					infoPrintln(reflex.id, err)
				}
				stdoutErr = nil
			case err := <-stderrErr:
				if err != nil {
					infoPrintln(reflex.id, err)
				}
				stderrErr = nil
			case err := <-cmdErr:
				if err != nil {
					stderr <- OutMsg{reflex.id, fmt.Sprintf("(error exit: %s)", err)}
				}
				cmdErr = nil
			}
			if stdoutErr == nil && stderrErr == nil && cmdErr == nil {
				break
			}
		}
		done <- struct{}{}
		if flagSequential {
			seqCommands.Unlock()
		}
	}()
}

func printOutput(out <-chan OutMsg, writer io.Writer) {
	for msg := range out {
		tag := ""
		if decoration == DecorationFancy || decoration == DecorationPlain {
			if msg.reflexID < 0 {
				tag = "[info]"
			} else {
				tag = fmt.Sprintf("[%02d]", msg.reflexID)
			}
		}

		if decoration == DecorationFancy {
			color := (msg.reflexID % numColors) + colorStart
			if reflexID < 0 {
				color = 31 // red
			}
			fmt.Fprintf(writer, "\x1b[01;%dm%s ", color, tag)
		} else if decoration == DecorationPlain {
			fmt.Fprintf(writer, tag+" ")
		}
		fmt.Fprint(writer, msg.message)
		if decoration == DecorationFancy {
			fmt.Fprintf(writer, "\x1b[m")
		}
		fmt.Fprintln(writer)
	}
}