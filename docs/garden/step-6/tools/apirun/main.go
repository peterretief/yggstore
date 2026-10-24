// apirun runs a site's server code the way the web nodes are planned to
// (docs/site-code.md in yggstore, stage 1): the handler, built for WASI, is
// compiled once, then the request is passed in CGI's environment variables
// and stdin to a fresh instance, with 64 MB and 2 seconds, and the reply
// read from its stdout. The web nodes don't run handlers yet; this is for
// trying one on your own computer.
//
//	go run . HANDLER.wasm METHOD PATH [BODY]
//	go run . ../../api/register.wasm POST /api/register '{"name":"Anna","email":"anna@example.org"}'
package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: go run . HANDLER.wasm METHOD PATH [BODY]")
		os.Exit(2)
	}
	file, method, uri, body := os.Args[1], os.Args[2], os.Args[3], ""
	if len(os.Args) > 4 {
		body = os.Args[4]
	}
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithMemoryLimitPages(1024).WithCloseOnContextDone(true))
	defer rt.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)
	bin, err := os.ReadFile(file)
	if err != nil {
		fail(err)
	}
	mod, err := rt.CompileModule(ctx, bin)
	if err != nil {
		fail(err)
	}
	path, query, _ := strings.Cut(uri, "?")
	var out bytes.Buffer
	cfg := wazero.NewModuleConfig().WithStdin(strings.NewReader(body)).WithStdout(&out).WithStderr(os.Stderr).WithName("").
		WithEnv("GATEWAY_INTERFACE", "CGI/1.1").WithEnv("SERVER_PROTOCOL", "HTTP/1.1").
		WithEnv("REQUEST_METHOD", method).WithEnv("REQUEST_URI", uri).WithEnv("SCRIPT_NAME", "").
		WithEnv("PATH_INFO", path).WithEnv("QUERY_STRING", query).
		WithEnv("HTTP_HOST", "localhost").WithEnv("SERVER_NAME", "localhost").WithEnv("SERVER_PORT", "443").WithEnv("HTTPS", "on").
		WithEnv("REMOTE_ADDR", "127.0.0.1").WithEnv("CONTENT_TYPE", "application/json").WithEnv("CONTENT_LENGTH", strconv.Itoa(len(body)))
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	t := time.Now()
	m, err := rt.InstantiateModule(c, mod, cfg)
	took := time.Since(t).Round(time.Millisecond)
	if m != nil {
		m.Close(ctx)
	}
	if err != nil && !strings.Contains(err.Error(), "exit_code(0)") {
		fail(err)
	}
	tp := textproto.NewReader(bufio.NewReader(&out))
	h, _ := tp.ReadMIMEHeader()
	status := "200"
	if s := h.Get("Status"); s != "" {
		status = s
	}
	rest, _ := tp.R.ReadString(0)
	fmt.Printf("%s %s -> %s, %s (%v)\n%s\n", method, uri, status, h.Get("Content-Type"), took, strings.TrimSpace(rest))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "apirun:", err)
	os.Exit(1)
}
