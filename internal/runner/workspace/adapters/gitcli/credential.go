package gitcli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// HelperCommand is the ai-runner subcommand git runs as its credential helper:
// `ai-runner git-credential <socket> <action>`.
//
// While one git command runs, the runner serves the token on a unix socket in a fresh 0700 temp directory (only
// the runner's user can connect; the socket holds no data). The helper forwards git's request (stdin) to the
// socket and prints the answer (stdout); the runner answers only a "get" for the configured protocol and host. So
// the token is never on a command line, in an environment variable, in a file, in the remote URL or in a log.
const HelperCommand = "git-credential"

// TokenUser is the user name of a GitHub App installation token.
const TokenUser = "x-access-token"

const maxCredentialIO = 64 << 10

type credential struct {
	scheme, host, token string
}

// answer is the reply to one helper request.
func (c credential) answer(r io.Reader) string {
	attrs := map[string]string{}
	sc := bufio.NewScanner(io.LimitReader(r, maxCredentialIO))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			if _, dup := attrs[k]; !dup {
				attrs[k] = v
			}
		}
	}
	if attrs["action"] != "get" || attrs["protocol"] != c.scheme || !strings.EqualFold(attrs["host"], c.host) {
		return ""
	}
	return "username=" + TokenUser + "\npassword=" + c.token + "\n"
}

type credServer struct {
	dir  string
	ln   net.Listener
	wg   sync.WaitGroup
	once sync.Once
}

// serve starts answering helper requests until close (or ctx ends).
func serve(ctx context.Context, c credential) (*credServer, error) {
	dir, err := os.MkdirTemp("", "kdv-git-") // mode 0700
	if err != nil {
		return nil, err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", filepath.Join(dir, "s"))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	s := &credServer{dir: dir, ln: ln}
	s.wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Go(func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
				_, _ = io.WriteString(conn, c.answer(conn))
			})
		}
	})
	context.AfterFunc(ctx, s.close)
	return s, nil
}

func (s *credServer) socket() string { return filepath.Join(s.dir, "s") }

// helperConfig is the credential.helper value: a shell snippet git runs with the action appended.
func (s *credServer) helperConfig(helper []string) string {
	q := make([]string, 0, len(helper)+1)
	for _, a := range append(append([]string{}, helper...), s.socket()) {
		q = append(q, "'"+a+"'")
	}
	return "!" + strings.Join(q, " ")
}

func (s *credServer) close() {
	s.once.Do(func() {
		_ = s.ln.Close()
		s.wg.Wait()
		_ = os.RemoveAll(s.dir)
	})
}

// HelperMain is the credential helper process: args are <socket> <action>. Only "get" is forwarded; "store" and
// "erase" are ignored (nothing is ever stored).
func HelperMain(args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) != 2 {
		return 2
	}
	if args[1] != "get" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	//nolint:gosec // G704: the runner's own unix socket, named in the helper command it gave git
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", args[0])
	if err != nil {
		return 1
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(conn, "action=get\n"); err != nil {
		return 1
	}
	if _, err := io.Copy(conn, io.LimitReader(stdin, maxCredentialIO)); err != nil {
		return 1
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		if err := uc.CloseWrite(); err != nil {
			return 1
		}
	}
	if _, err := io.Copy(stdout, io.LimitReader(conn, maxCredentialIO)); err != nil && !errors.Is(err, io.EOF) {
		return 1
	}
	return 0
}
