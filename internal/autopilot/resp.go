package autopilot

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// A minimal RESP2 client, enough to ask Valkey's Sentinels about the replication group
// (AUTH, SENTINEL GET-MASTER-ADDR-BY-NAME, SENTINEL REPLICAS). The operator writes no data
// to Valkey and needs no connection pool, so it does not depend on a Redis client library.

// respTimeout bounds one Sentinel conversation (dial, AUTH and the queries), dialTimeout the
// connection: a Sentinel in the cluster accepts within milliseconds, while a NetworkPolicy
// that drops the packets would otherwise hold the controller's worker for all of respTimeout.
const (
	respTimeout = 3 * time.Second
	dialTimeout = time.Second
)

// respError is an error reply ("-ERR ...", "-NOAUTH ...").
type respError string

func (e respError) Error() string { return string(e) }

// sentinelConn is one connection to a Sentinel.
type sentinelConn struct {
	conn net.Conn
	r    *bufio.Reader
}

// dialSentinel connects to addr and authenticates with password (when set).
func dialSentinel(ctx context.Context, addr, password string) (*sentinelConn, error) {
	ctx, cancel := context.WithTimeout(ctx, respTimeout)
	defer cancel()
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// Whatever the peer sends, the operator reads at most maxRESPBytes from it.
	c := &sentinelConn{conn: conn, r: bufio.NewReader(io.LimitReader(conn, maxRESPBytes))}
	if password != "" {
		if _, err := c.do("AUTH", password); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("AUTH: %w", err)
		}
	}
	return c, nil
}

func (c *sentinelConn) Close() error { return c.conn.Close() }

// do sends one command and reads its reply: string, int64, nil, []any, or a respError.
func (c *sentinelConn) do(args ...string) (any, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := io.WriteString(c.conn, b.String()); err != nil {
		return nil, err
	}
	return readRESP(c.r)
}

// Bounds of what a peer can make the operator read and allocate. The operator dials only the
// pods of its own Valkey StatefulSet, but their image is the user's choice
// (spec.highAvailability.valkey.image), and one operator serves every namespace. The replies
// it asks for are small: SENTINEL REPLICAS of two replicas is about 3 KB, two levels deep.
const (
	// maxRESPBytes bounds everything read from one connection (AUTH and the queries).
	maxRESPBytes = 256 << 10
	// maxRESPElements bounds an array, maxRESPBulk a bulk string.
	maxRESPElements = 1024
	maxRESPBulk     = 64 << 10
	// maxRESPDepth bounds the nesting of arrays.
	maxRESPDepth = 4
)

func readRESP(r *bufio.Reader) (any, error) {
	return readRESPAt(r, 0)
}

func readRESPAt(r *bufio.Reader, depth int) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if line == "" {
		return nil, errors.New("empty RESP line")
	}
	body := line[1:]
	switch line[0] {
	case '+':
		return body, nil
	case '-':
		return nil, respError(body)
	case ':':
		return strconv.ParseInt(body, 10, 64)
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil || n > maxRESPBulk {
			return nil, fmt.Errorf("bad bulk length %q", body)
		}
		if n < 0 {
			return nil, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(body)
		if err != nil || n > maxRESPElements {
			return nil, fmt.Errorf("bad array length %q", body)
		}
		if n < 0 {
			return nil, nil
		}
		if depth >= maxRESPDepth {
			return nil, errors.New("RESP arrays nested too deep")
		}
		// No capacity from the declared length: memory grows only with what was read.
		var out []any
		for range n {
			v, err := readRESPAt(r, depth+1)
			if err != nil {
				var re respError
				if !errors.As(err, &re) {
					return nil, err
				}
				v = re
			}
			out = append(out, v)
		}
		if out == nil {
			out = []any{}
		}
		return out, nil
	}
	return nil, fmt.Errorf("unexpected RESP type %q", line[0])
}

// respStrings converts an array reply of strings.
func respStrings(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, _ := e.(string)
		out = append(out, s)
	}
	return out
}

// respMaps converts an array of flat field/value arrays (SENTINEL REPLICAS) into maps.
func respMaps(v any) []map[string]string {
	arr, _ := v.([]any)
	out := make([]map[string]string, 0, len(arr))
	for _, e := range arr {
		kv := respStrings(e)
		m := make(map[string]string, len(kv)/2)
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		out = append(out, m)
	}
	return out
}
