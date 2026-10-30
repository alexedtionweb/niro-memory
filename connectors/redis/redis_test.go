package redis_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-memory/connectors/redis"
)

// fake is a tiny RESP server supporting AUTH, SELECT, GET and SET … PX.
func fake(t *testing.T, password string) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	data := map[string]string{}
	exp := map[string]time.Time{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				authed := password == ""
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
					args := make([]string, n)
					for i := range args {
						l, _ := r.ReadString('\n')
						size, _ := strconv.Atoi(strings.TrimSpace(l[1:]))
						buf := make([]byte, size+2)
						io.ReadFull(r, buf)
						args[i] = string(buf[:size])
					}
					mu.Lock()
					switch {
					case args[0] == "AUTH":
						authed = args[1] == password
						io.WriteString(c, map[bool]string{true: "+OK\r\n", false: "-WRONGPASS\r\n"}[authed])
					case !authed:
						io.WriteString(c, "-NOAUTH\r\n")
					case args[0] == "SELECT":
						io.WriteString(c, "+OK\r\n")
					case args[0] == "SET":
						data[args[1]] = args[2]
						ms, _ := strconv.Atoi(args[4])
						exp[args[1]] = time.Now().Add(time.Duration(ms) * time.Millisecond)
						io.WriteString(c, "+OK\r\n")
					case args[0] == "GET":
						v, ok := data[args[1]]
						if !ok || time.Now().After(exp[args[1]]) {
							io.WriteString(c, "$-1\r\n")
						} else {
							io.WriteString(c, "$"+strconv.Itoa(len(v))+"\r\n"+v+"\r\n")
						}
					}
					mu.Unlock()
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

func TestCache(t *testing.T) {
	addr := fake(t, "s3cret")
	if real := os.Getenv("NIROMEM_REDIS_URL"); real != "" {
		t.Logf("also see TestRealRedis")
	}
	c, err := redis.New(redis.Options{URL: "redis://:s3cret@" + addr + "/2"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, ok := c.Get(ctx, "k"); ok {
		t.Fatal("hit on empty cache")
	}
	c.Set(ctx, "k", []byte("bin\x00ary\r\n"), time.Minute)
	if v, ok := c.Get(ctx, "k"); !ok || string(v) != "bin\x00ary\r\n" {
		t.Fatalf("%q %v", v, ok)
	}
	c.Set(ctx, "short", []byte("x"), 10*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	if _, ok := c.Get(ctx, "short"); ok {
		t.Fatal("ttl")
	}
	var errs []error
	bad, _ := redis.New(redis.Options{URL: "redis://:wrong@" + addr, OnError: func(e error) { errs = append(errs, e) }})
	if _, ok := bad.Get(ctx, "k"); ok || len(errs) == 0 {
		t.Fatal("auth failure must be a reported miss")
	}
	down, _ := redis.New(redis.Options{URL: "redis://127.0.0.1:1"})
	if _, ok := down.Get(ctx, "k"); ok {
		t.Fatal("unreachable cache must be a miss")
	}
}

// Set NIROMEM_REDIS_URL to run against a real server.
func TestRealRedis(t *testing.T) {
	u := os.Getenv("NIROMEM_REDIS_URL")
	if u == "" {
		t.Skip("NIROMEM_REDIS_URL not set")
	}
	c, err := redis.New(redis.Options{URL: u, Prefix: "niromem-test:"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c.Set(ctx, "k", []byte("v"), time.Minute)
	if v, ok := c.Get(ctx, "k"); !ok || string(v) != "v" {
		t.Fatal("round trip")
	}
}
