// SPDX-License-Identifier: Apache-2.0

package record

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
)

// ack is what the recorder answers a hook payload with once it is recorded.
const ack = "recorded\n"

// hookMain is `record-session hook SOCKET EVENT`, the hook command the
// recorder configures for each event. It hands the payload on stdin to the
// recorder listening on SOCKET and waits until it is recorded. It prints
// nothing on stdout, because Claude Code adds what some hooks print to the
// session.
func hookMain(args []string, stdin io.Reader, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "usage: record-session hook SOCKET EVENT")
		return 2
	}
	socket, event := args[0], args[1]
	payload, err := io.ReadAll(stdin)
	if err == nil {
		err = deliver(socket, event, payload)
	}
	if err != nil {
		fmt.Fprintf(stderr, "record-session: %s hook: %v\n", event, err)
		return 1
	}
	return 0
}

func deliver(socket, event string, payload []byte) error {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s\n%s", event, payload); err != nil {
		return err
	}
	conn.(*net.UnixConn).CloseWrite()
	reply, err := io.ReadAll(conn)
	if err != nil {
		return err
	}
	if string(reply) != ack {
		return fmt.Errorf("the recorder did not record the payload")
	}
	return nil
}

// serveHooks accepts each hook command's connection, sends its payload as an
// event and answers once the event is recorded.
func serveHooks(listener net.Listener, send func(event) bool, stop <-chan struct{}) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			r := bufio.NewReader(conn)
			name, err := r.ReadString('\n')
			if err != nil {
				return
			}
			payload, err := io.ReadAll(r)
			if err != nil {
				return
			}
			e := event{kind: kindHook, hook: strings.TrimSuffix(name, "\n"), data: payload, done: make(chan struct{})}
			if !send(e) {
				return
			}
			select {
			case <-e.done:
				io.WriteString(conn, ack)
			case <-stop:
			}
		}()
	}
}
