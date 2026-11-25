//go:build unix

package cliagent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/app"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// Agents is the session executor's AI CLIs (app.Agents, KIN-79): the configured adapters by agent name. The CLI
// runs as `ai-runner agent-exec` (ExecMain) started by the attempt launcher, so it is adopted after a restart like
// any attempt process; Follow reads the output files it writes.
type Agents struct {
	Adapters map[string]*Adapter
	// Poll is how often a follower looks for new output (DefaultPoll when zero) and Save how often it reports the
	// offsets reached (DefaultSave when zero).
	Poll, Save time.Duration
}

var _ app.Agents = (*Agents)(nil)

// Defaults of Agents.
const (
	DefaultPoll = 100 * time.Millisecond
	DefaultSave = time.Second
)

// Prepare writes the prompt to PromptFile in dir (outside the worktree), creates dir/work as the working directory
// when the request has none, validates the request and builds the CLI's command.
func (g *Agents) Prepare(dir string, req domain.AgentRequest, prompt string, p domain.Permissions) (app.AgentCommand, error) {
	a := g.Adapters[req.Agent]
	if a == nil {
		return app.AgentCommand{}, fmt.Errorf("%w: %s", app.ErrNoAgent, req.Agent)
	}
	if req.Workdir == "" {
		req.Workdir = filepath.Join(dir, "work")
		if err := os.MkdirAll(req.Workdir, 0o700); err != nil {
			return app.AgentCommand{}, fmt.Errorf("creating the working directory: %w", err)
		}
	}
	req.PromptFile = filepath.Join(dir, PromptFile)
	if err := writePrompt(req.PromptFile, prompt); err != nil {
		return app.AgentCommand{}, err
	}
	c, err := a.Command(req, p)
	if err != nil {
		return app.AgentCommand{}, err
	}
	return app.AgentCommand{Path: c.Path, Args: c.Args, Env: c.Env, Dir: c.Dir, PromptFile: req.PromptFile,
		Timeout: req.Timeout}, nil
}

func writePrompt(name, prompt string) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // the attempt's own file
	if err != nil {
		return fmt.Errorf("writing the prompt: %w", err)
	}
	if _, err := io.WriteString(f, prompt); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing the prompt: %w", err)
	}
	return f.Close()
}

// Follow starts reading the output files of the attempt's CLI in dir from their start: every line is parsed into
// the run's transcript; events, raw output and offsets beyond h.From go to h.
func (g *Agents) Follow(agent, dir string, h app.AgentFollow) (app.AgentFollower, error) {
	d, err := DialectFor(agent)
	if err != nil {
		return nil, err
	}
	poll, save := g.Poll, g.Save
	if poll <= 0 {
		poll = DefaultPoll
	}
	if save <= 0 {
		save = DefaultSave
	}
	maxLine := DefaultMaxLine
	if a := g.Adapters[agent]; a != nil && a.Config.MaxLine > 0 {
		maxLine = a.Config.MaxLine
	}
	f := &follower{dir: dir, h: h, parser: d.Parser(), maxLine: maxLine, poll: poll, save: save,
		stop: make(chan struct{}), done: make(chan struct{})}
	f.out.from, f.errs.from = h.From.Stdout, h.From.Stderr
	go f.run()
	return f, nil
}

// tracked is one output file being read.
type tracked struct {
	file *os.File
	pos  int64 // bytes read
	from int64 // bytes reported before (a restart): not sent again
}

type follower struct {
	dir     string
	h       app.AgentFollow
	parser  Parser
	maxLine int
	poll    time.Duration
	save    time.Duration

	out, errs tracked
	line      []byte
	skipping  bool
	tr        domain.Transcript
	tail      tailBuffer
	saved     domain.OutputOffsets
	lastSave  time.Time

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

func (f *follower) run() {
	defer close(f.done)
	defer f.closeFiles()
	t := time.NewTicker(f.poll)
	defer t.Stop()
	for {
		f.readAll()
		f.progress(false)
		select {
		case <-f.stop:
			f.readAll() // the process ended: what it wrote is complete
			f.flushLine()
			f.progress(true)
			return
		case <-t.C:
		}
	}
}

func (f *follower) closeFiles() {
	for _, t := range []*tracked{&f.out, &f.errs} {
		if t.file != nil {
			_ = t.file.Close()
		}
	}
}

func (f *follower) readAll() {
	f.read(&f.out, StdoutFile, f.h.Stdout, f.lines)
	f.read(&f.errs, StderrFile, f.h.Stderr, func(b []byte, _ int64) { _, _ = f.tail.Write(b) })
}

// read passes what was appended to the file since the last read to consume (all of it, for the transcript) and the
// part beyond t.from to raw.
func (f *follower) read(t *tracked, name string, raw io.Writer, consume func([]byte, int64)) {
	if t.file == nil {
		fh, err := os.OpenFile(filepath.Join(f.dir, name), os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // the attempt's own file
		if err != nil {
			return // not written yet
		}
		t.file = fh
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := t.file.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			start := t.pos
			t.pos += int64(n)
			consume(chunk, start)
			if raw != nil && t.pos > t.from {
				skip := max(t.from-start, 0)
				_, _ = raw.Write(chunk[skip:])
			}
		}
		if err != nil || n == 0 { // EOF: the rest comes with the next poll
			return
		}
	}
}

// lines splits stdout into lines (start is the chunk's offset) and parses each complete one; events of a line that
// ends beyond the reported offset are sent.
func (f *follower) lines(chunk []byte, start int64) {
	for len(chunk) > 0 {
		i := indexNL(chunk)
		part := chunk
		if i >= 0 {
			part = chunk[:i+1]
		}
		chunk = chunk[len(part):]
		start += int64(len(part))
		if !f.skipping {
			if len(f.line)+len(part) > f.maxLine {
				f.skipping, f.line = true, f.line[:0]
			} else {
				f.line = append(f.line, part...)
			}
		}
		if i >= 0 {
			if !f.skipping && len(f.line) > 0 {
				f.parse(f.line, start > f.out.from)
			}
			f.line, f.skipping = f.line[:0], false
		}
	}
}

func indexNL(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}

// flushLine parses a last line without a newline.
func (f *follower) flushLine() {
	if !f.skipping && len(f.line) > 0 {
		f.parse(f.line, f.out.pos > f.out.from)
	}
	f.line, f.skipping = f.line[:0], false
}

func (f *follower) parse(line []byte, send bool) {
	for _, e := range f.parser.Parse(line) {
		f.tr.Apply(e)
		if send && f.h.OnEvent != nil {
			f.h.OnEvent(e)
		}
	}
}

// progress reports the offsets reached (now and then, and at the end).
func (f *follower) progress(final bool) {
	o := domain.OutputOffsets{Stdout: max(f.out.pos, f.out.from), Stderr: max(f.errs.pos, f.errs.from)}
	if f.h.Progress == nil || o == f.saved || (!final && time.Since(f.lastSave) < f.save) {
		return
	}
	f.saved, f.lastSave = o, time.Now()
	f.h.Progress(o)
}

// Finish reads the rest of the output and returns the run's result: how the CLI ended comes from agent-exec's
// record when it left one (exit is then only the attempt process's), the cancel reason from the executor.
func (f *follower) Finish(exit domain.Exit, cancelled string) domain.AgentResult {
	f.stopOnce.Do(func() { close(f.stop) })
	<-f.done
	end := domain.End{Exit: exit, StderrTail: f.tail.String(), Cancelled: cancelled}
	if e, ok := ReadEnd(f.dir); ok {
		end.Exit, end.TimedOut = domain.Exit{Code: e.Code, Signaled: e.Signaled}, e.TimedOut
	}
	return f.tr.Result(end)
}
