package redact

import (
	"bytes"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// MinKnownLen is the shortest known secret value masked by exact match: a shorter one ("test", "1234") would mask
// ordinary words everywhere and reveal itself by the pattern of the masks.
const MinKnownLen = 6

// MaxCarry bounds the partial line a Stream holds back (per stream): a longer line is emitted in pieces cut
// between words.
const MaxCarry = 8 << 10

var (
	pemBeginRe = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	pemEndRe   = regexp.MustCompile(`-----END [A-Z ]*PRIVATE KEY-----`)
)

// Lines redacts process output line by line: Text on every line, every occurrence of a known secret value (at
// least MinKnownLen bytes) and every line of a PEM private key block. Lines, their order and their endings ("\n",
// "\r\n") are kept, so the output never gains or loses lines; a changed line keeps its indentation.
func Lines(s string, known ...string) string {
	l := newLiner(known)
	var b strings.Builder
	b.Grow(len(s))
	l.appendText(&b, s)
	return b.String()
}

// liner redacts lines; it carries the "inside a PEM block" state from one line to the next.
type liner struct {
	known    []string // longest first, so a value containing another is masked whole
	inPEM    bool
	pemLines int // lines masked in the current PEM block
}

// maxPEMLines ends a PEM block that has no END line (a truncated key, a header echoed by a script), so it cannot
// mask the rest of the output; a 4096-bit RSA key has about 50 lines.
const maxPEMLines = 100

func newLiner(known []string) *liner {
	l := &liner{}
	for _, k := range known {
		if len(strings.TrimSpace(k)) >= MinKnownLen && !slices.Contains(l.known, k) {
			l.known = append(l.known, k)
		}
	}
	slices.SortFunc(l.known, func(a, b string) int { return len(b) - len(a) })
	return l
}

// appendText redacts s (lines and possibly a trailing partial line) into b.
func (l *liner) appendText(b *strings.Builder, s string) {
	for s != "" {
		line, rest, nl := strings.Cut(s, "\n")
		b.WriteString(l.line(line))
		if nl {
			b.WriteByte('\n')
		}
		s = rest
	}
}

// line redacts one line without its "\n".
func (l *liner) line(s string) string {
	cr := ""
	if strings.HasSuffix(s, "\r") {
		s, cr = s[:len(s)-1], "\r"
	}
	if l.inPEM {
		l.pemLines++
		switch {
		case strings.TrimSpace(s) == "": // keys have no blank lines: the block ended without END
			l.inPEM = false
			return s + cr
		case pemEndRe.MatchString(s), l.pemLines >= maxPEMLines:
			l.inPEM = false
		}
		return Placeholder + cr
	}
	if loc := pemBeginRe.FindStringIndex(s); loc != nil && !pemEndRe.MatchString(s[loc[1]:]) {
		l.inPEM, l.pemLines = true, 0 // Text masks this line from BEGIN on; the next lines are the key
	}
	out := s
	for _, k := range l.known {
		if strings.Contains(out, k) {
			out = strings.ReplaceAll(out, k, Placeholder)
		}
	}
	if red := Text(out); red != out {
		// Text joins the words with single spaces; keep the indentation of code and logs and trailing blanks (a
		// partial line goes on after them).
		out = s[:len(s)-len(strings.TrimLeft(s, " \t"))] + red + s[len(strings.TrimRight(s, " \t")):]
	}
	return out + cr
}

// Stream is an io.Writer that redacts process output (see Lines) before passing it to the next writer. Secrets
// split across writes are masked: only complete lines are passed on, the partial last line is held back (a sliding
// window of at most MaxCarry bytes) until its newline arrives or Flush is called. While the stream is idle the
// partial line is passed on except a word still being written and a trailing key or auth scheme waiting for its
// value (a "Password: " prompt shows once the process writes more or ends). A line longer than MaxCarry is passed on
// in pieces cut between words, holding back its last words, or (no blanks) holding back a tail at least as long as
// the longest known value, so a "password: value" pair or a token is not cut in two. Memory is bounded by MaxCarry plus one write; the time is linear in the output.
type Stream struct {
	mu    sync.Mutex
	w     io.Writer
	l     *liner
	carry []byte
	idle  time.Duration
	timer *time.Timer
	b     strings.Builder
}

// NewStream redacts what is written to it into w. idle > 0 passes a held-back partial line on after that much
// time without writes (a prompt or progress line stays visible live); 0 holds it until a newline or Flush.
// known are exact secret values the caller holds (a git token, a proxy password).
func NewStream(w io.Writer, idle time.Duration, known ...string) *Stream {
	return &Stream{w: w, l: newLiner(known), idle: idle}
}

// Write redacts p's complete lines into the next writer and holds back the partial last line. It reports all of
// p as written unless the next writer fails.
func (s *Stream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.carry = append(s.carry, p...)
	end := bytes.LastIndexByte(s.carry, '\n') + 1
	if len(s.carry)-end > MaxCarry {
		end += cutPoint(s.carry[end:], s.l.tail())
	}
	err := s.emit(end)
	if s.idle > 0 && len(s.carry) > 0 {
		if s.timer == nil {
			s.timer = time.AfterFunc(s.idle, s.idleFlush)
		} else {
			s.timer.Reset(s.idle)
		}
	}
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush passes the held-back partial line on (at the end of the process, before its terminal event).
func (s *Stream) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
	}
	return s.emit(len(s.carry))
}

// idleFlush passes the partial line on while the process is quiet, except what may be the start of a secret.
func (s *Stream) idleFlush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.emit(idleCut(s.carry)) // a failing writer fails the next Write too
}

// emit redacts carry[:n] into the next writer and keeps the rest.
func (s *Stream) emit(n int) error {
	if n == 0 {
		return nil
	}
	s.b.Reset()
	s.l.appendText(&s.b, string(s.carry[:n]))
	rest := copy(s.carry, s.carry[n:])
	s.carry = s.carry[:rest]
	if cap(s.carry) > 2*MaxCarry { // a large write grew it: give the memory back
		s.carry = append([]byte(nil), s.carry...)
	}
	if s.b.Len() == 0 {
		return nil
	}
	_, err := io.WriteString(s.w, s.b.String())
	return err
}

// buffered is the number of bytes held back.
func (s *Stream) buffered() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.carry) }

// holdWords is how many trailing words a cut line holds back ("Authorization: Bearer <token>").
const holdWords = 3

// tail is how much of a line without blanks a cut holds back: enough for the longest known value or a token.
func (l *liner) tail() int {
	n := minTail
	if len(l.known) > 0 {
		n = max(n, len(l.known[0]))
	}
	return min(n, MaxCarry/2)
}

// minTail is the least a cut inside a word holds back.
const minTail = 512

// idleCut is how much of a quiet partial line an idle flush passes on: all of it but a word still being written
// (no blank after it) and a trailing key or auth scheme ("password:", "Bearer") whose value has not arrived.
func idleCut(b []byte) int {
	i := len(b)
	for i > 0 && !isBlank(b[i-1]) { // a word still being written
		i--
	}
	j := i
	for j > 0 && isBlank(b[j-1]) {
		j--
	}
	k := j
	for k > 0 && !isBlank(b[k-1]) {
		k--
	}
	if w := string(b[k:j]); w != "" && (isScheme(w) || strings.ContainsAny(w[len(w)-1:], ":=") &&
		SecretKey(strings.Trim(w[:len(w)-1], "\"'{"))) {
		return k
	}
	return i
}

// cutPoint is where a partial line longer than MaxCarry is cut: before its last holdWords words when they fit in
// half the carry, else before its last word, else (one word longer than that) tail bytes before its end, at a
// character boundary.
func cutPoint(b []byte, tail int) int {
	i := len(b)
	for w := 0; w < holdWords && i > 0; w++ {
		for i > 0 && !isBlank(b[i-1]) { // the word
			i--
		}
		for i > 0 && w < holdWords-1 && isBlank(b[i-1]) { // the blanks before it
			i--
		}
	}
	if i > 0 && len(b)-i <= MaxCarry/2 {
		return i
	}
	i = len(b)
	for i > 0 && !isBlank(b[i-1]) {
		i--
	}
	if i > 0 && len(b)-i <= MaxCarry/2 {
		return i
	}
	i = len(b) - tail
	for i > 0 && !utf8.RuneStart(b[i]) {
		i--
	}
	return i
}

func isBlank(c byte) bool { return c == ' ' || c == '\t' }
