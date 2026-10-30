// Package redis is a cache.Cache connector for Redis (or Valkey, KeyDB)
// with a minimal RESP client on the standard library — no dependencies.
//
// The cache is never authoritative: errors are treated as misses and
// logged by the caller's observer, never surfaced as failures.
package redis

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/alexedtionweb/niro-memory/cache"
)

// Options configures the client.
type Options struct {
	// URL is redis://[:password@]host:port[/db].
	URL string
	// Prefix namespaces keys (default "niromem:").
	Prefix  string
	Pool    int           // idle connections kept (default 16)
	Timeout time.Duration // per command (default 200ms: a slow cache is a miss)
	// OnError, if set, receives connection and protocol errors.
	OnError func(error)
}

// Cache is a Redis-backed cache.Cache.
type Cache struct {
	o        Options
	addr     string
	password string
	db       int
	idle     chan *conn
}

var _ cache.Cache = (*Cache)(nil)

type conn struct {
	net.Conn
	r *bufio.Reader
}

// New parses the URL. Connections are opened lazily.
func New(o Options) (*Cache, error) {
	u, err := url.Parse(o.URL)
	if err != nil || u.Scheme != "redis" || u.Host == "" {
		return nil, fmt.Errorf("redis: url must be redis://[:password@]host:port[/db]")
	}
	c := &Cache{o: o, addr: u.Host}
	if p, ok := u.User.Password(); ok {
		c.password = p
	}
	if db := strings.TrimPrefix(u.Path, "/"); db != "" {
		if c.db, err = strconv.Atoi(db); err != nil {
			return nil, fmt.Errorf("redis: bad db %q", db)
		}
	}
	if c.o.Prefix == "" {
		c.o.Prefix = "niromem:"
	}
	if c.o.Pool <= 0 {
		c.o.Pool = 16
	}
	if c.o.Timeout <= 0 {
		c.o.Timeout = 200 * time.Millisecond
	}
	c.idle = make(chan *conn, c.o.Pool)
	return c, nil
}

// Close closes idle connections.
func (c *Cache) Close() error {
	for {
		select {
		case cn := <-c.idle:
			cn.Close()
		default:
			return nil
		}
	}
}

func (c *Cache) get() (*conn, error) {
	select {
	case cn := <-c.idle:
		return cn, nil
	default:
	}
	nc, err := net.DialTimeout("tcp", c.addr, c.o.Timeout)
	if err != nil {
		return nil, err
	}
	cn := &conn{Conn: nc, r: bufio.NewReader(nc)}
	cn.SetDeadline(time.Now().Add(c.o.Timeout))
	if c.password != "" {
		if _, err := cn.do("AUTH", c.password); err != nil {
			cn.Close()
			return nil, err
		}
	}
	if c.db != 0 {
		if _, err := cn.do("SELECT", strconv.Itoa(c.db)); err != nil {
			cn.Close()
			return nil, err
		}
	}
	return cn, nil
}

func (c *Cache) put(cn *conn) {
	select {
	case c.idle <- cn:
	default:
		cn.Close()
	}
}

func (c *Cache) run(ctx context.Context, args ...string) ([]byte, error) {
	cn, err := c.get()
	if err != nil {
		return nil, err
	}
	dl := time.Now().Add(c.o.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(dl) {
		dl = d
	}
	cn.SetDeadline(dl)
	v, err := cn.do(args...)
	var re respError
	if err != nil && !errors.As(err, &re) {
		cn.Close() // the connection state is unknown
		return nil, err
	}
	c.put(cn)
	return v, err
}

type respError string

func (e respError) Error() string { return "redis: " + string(e) }

// do writes a command as a RESP array of bulk strings and reads one reply.
func (cn *conn) do(args ...string) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := io.WriteString(cn, b.String()); err != nil {
		return nil, err
	}
	line, err := cn.r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSuffix(line, "\r\n")
	if line == "" {
		return nil, errors.New("redis: empty reply")
	}
	switch line[0] {
	case '+', ':':
		return []byte(line[1:]), nil
	case '-':
		return nil, respError(line[1:])
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, nil // nil bulk string: missing key
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(cn.r, buf); err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
	return nil, fmt.Errorf("redis: unexpected reply %q", line)
}

func (c *Cache) fail(err error) {
	if c.o.OnError != nil && err != nil {
		c.o.OnError(err)
	}
}

// Get returns a cached value; any error is a miss.
func (c *Cache) Get(ctx context.Context, key string) ([]byte, bool) {
	v, err := c.run(ctx, "GET", c.o.Prefix+key)
	if err != nil {
		c.fail(err)
		return nil, false
	}
	return v, v != nil
}

// Set stores a value with a TTL; failures are ignored (reported to OnError).
func (c *Cache) Set(ctx context.Context, key string, val []byte, ttl time.Duration) {
	ms := ttl.Milliseconds()
	if ms <= 0 {
		ms = 1
	}
	_, err := c.run(ctx, "SET", c.o.Prefix+key, string(val), "PX", strconv.FormatInt(ms, 10))
	c.fail(err)
}
