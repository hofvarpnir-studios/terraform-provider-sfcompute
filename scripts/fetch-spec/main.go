// Command fetch-spec rebuilds spec/openapi.json from the OpenAPI slices that
// SF Compute's documentation embeds in each API reference page.
//
// SF Compute does not publish its OpenAPI document at a public URL, but every
// page listed in https://docs.sfcompute.com/llms.txt under api-reference/
// contains the slice of the document for that endpoint (the operation plus
// every component it references). This program downloads them all, merges
// them, fails on any conflicting duplicate definition, and writes the result
// with sorted keys so that diffs between snapshots are minimal.
//
// Usage:
//
//	go run ./scripts/fetch-spec [-index URL] [-out spec]
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hofvarpnir-studios/terraform-provider-sfcompute/internal/specmerge"
)

func main() {
	index := flag.String("index", "https://docs.sfcompute.com/llms.txt", "documentation index (llms.txt) to discover API reference pages from")
	outDir := flag.String("out", "spec", "directory to write merged OpenAPI documents into; the source /openapi.json becomes <out>/openapi.json, /preview/openapi.json becomes <out>/preview/openapi.json")
	require := flag.String("require", "/openapi.json", "source document that must be present (comma-separated)")
	conc := flag.Int("concurrency", 6, "parallel page downloads")
	timeout := flag.Duration("timeout", 5*time.Minute, "overall timeout")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	f := &specmerge.Fetcher{
		Client:      &http.Client{Timeout: 60 * time.Second},
		Concurrency: *conc,
		Retries:     3,
		UserAgent:   "terraform-provider-sfcompute-fetch-spec (+https://github.com/hofvarpnir-studios/terraform-provider-sfcompute)",
		Logf:        logf,
	}
	docs, stats, err := specmerge.Build(ctx, f, *index)
	if err != nil {
		fatal(err)
	}
	for _, src := range strings.Split(*require, ",") {
		if src = strings.TrimSpace(src); src != "" && docs[src] == nil {
			fatal(fmt.Errorf("required source %s was not found in any page", src))
		}
	}
	sources := make([]string, 0, len(docs))
	for src := range docs {
		sources = append(sources, src)
	}
	sort.Strings(sources)
	for _, src := range sources {
		data, err := specmerge.Marshal(docs[src])
		if err != nil {
			fatal(err)
		}
		out := filepath.Join(*outDir, filepath.FromSlash(strings.TrimPrefix(src, "/")))
		if ext := filepath.Ext(out); ext != ".json" {
			out = strings.TrimSuffix(out, ext) + ".json"
		}
		st := stats[src]
		summary := fmt.Sprintf("%d pages, %d slices, %d operations, %d schemas", st.Pages, st.Slices, st.Operations, st.Schemas)
		if old, err := os.ReadFile(out); err == nil && bytes.Equal(old, data) {
			logf("%s -> %s unchanged (%s)", src, out, summary)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(out, data, 0o644); err != nil {
			fatal(err)
		}
		logf("%s -> wrote %s (%s)", src, out, summary)
	}
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fetch-spec: "+format+"\n", args...)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "fetch-spec: %v\n", err)
	os.Exit(1)
}
