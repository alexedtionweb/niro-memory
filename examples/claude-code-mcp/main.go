// Command niromem-mcp adapts an existing memory API to MCP.
// For an all-in-one deployment, use niromem serve -mcp instead.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/alexedtionweb/niro-memory/client"
	"github.com/alexedtionweb/niro-memory/mcp"
)

func main() {
	httpAddress := flag.String("http", "", "serve MCP on a loopback address (default: stdio); use niromem serve -mcp for all-in-one")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatal("unexpected arguments; use -h for usage")
	}
	baseURL := os.Getenv("NIROMEM_URL")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8080"
	}
	c := client.New(baseURL, os.Getenv("NIROMEM_API_KEY"))
	c.HTTP.Timeout = mcp.SearchTimeout + 5*time.Second
	s, err := mcp.NewWorkspace(c, os.Getenv("NIROMEM_USER_ID"), os.Getenv("NIROMEM_AGENT_ID"))
	if err != nil {
		log.Fatal(err)
	}
	if *httpAddress != "" {
		if err := serveHTTP(s, *httpAddress, baseURL, os.Getenv("NIROMEM_MCP_TOKEN")); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := s.ServeStdio(context.Background(), os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func localAddress(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("MCP HTTP requires a loopback address, got %q", address)
	}
	return net.JoinHostPort(host, port), nil
}

func validateBackendAddress(address, backend string) error {
	local, err := localAddress(address)
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(local)
	if err != nil {
		return err
	}
	u, err := url.Parse(backend)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return errors.New("NIROMEM_URL must be an HTTP(S) URL for the memory API")
	}
	backendPort := u.Port()
	if backendPort == "" {
		backendPort = "80"
		if u.Scheme == "https" {
			backendPort = "443"
		}
	}
	ip := net.ParseIP(u.Hostname())
	if port != "0" && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())) && port == backendPort {
		return fmt.Errorf("standalone MCP and API cannot share port %s; use niromem serve -mcp for all-in-one", port)
	}
	return nil
}

func serveHTTP(s *mcp.Server, address, backend, token string) error {
	if err := validateBackendAddress(address, backend); err != nil {
		return err
	}
	local, err := localAddress(address)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", local)
	if err != nil {
		return err
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler: s.HTTPHandler(port, token), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: mcp.SearchTimeout + 10*time.Second, IdleTimeout: 60 * time.Second,
	}
	log.Printf("MCP adapter at http://%s/mcp; memory API at %s", listener.Addr(), backend)
	if token == "" {
		log.Print("no MCP bearer token; trusted local development only")
	}
	return srv.Serve(listener)
}
