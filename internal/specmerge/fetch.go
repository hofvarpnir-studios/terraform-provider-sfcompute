package specmerge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fetcher downloads the documentation index and API reference pages.
type Fetcher struct {
	Client      *http.Client
	Concurrency int
	Retries     int
	UserAgent   string
	// Logf, if set, receives progress messages.
	Logf func(format string, args ...any)
}

func (f *Fetcher) logf(format string, args ...any) {
	if f.Logf != nil {
		f.Logf(format, args...)
	}
}

// validSource restricts fence sources to simple paths ending in openapi.json
// (or .yaml), because the source decides the output file name.
var validSource = regexp.MustCompile(`^(/[a-z0-9_-]+)*/openapi\.(json|ya?ml)$`)

var markdownLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// ReferencePages parses an llms.txt index and returns the absolute URLs of
// every API reference page (any link whose path contains "/api-reference/"
// and ends in ".md"), sorted and de-duplicated. Links to other hosts are
// ignored.
func ReferencePages(index []byte, indexURL string) ([]string, error) {
	base, err := url.Parse(indexURL)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var pages []string
	for _, m := range markdownLink.FindAllSubmatch(index, -1) {
		u, err := base.Parse(string(m[1]))
		if err != nil {
			continue
		}
		if u.Host != base.Host || !strings.Contains(u.Path, "/api-reference/") || !strings.HasSuffix(u.Path, ".md") {
			continue
		}
		u.Fragment, u.RawQuery = "", ""
		s := u.String()
		if !seen[s] {
			seen[s] = true
			pages = append(pages, s)
		}
	}
	sort.Strings(pages)
	return pages, nil
}

// Get fetches a URL, retrying transient failures.
func (f *Fetcher) Get(ctx context.Context, u string) ([]byte, error) {
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	var lastErr error
	for attempt := 0; attempt <= f.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if f.UserAgent != "" {
			req.Header.Set("User-Agent", f.UserAgent)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return body, nil
		}
		lastErr = fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			break
		}
	}
	return nil, lastErr
}

// Stats summarises a Build.
type Stats struct {
	Pages      int
	Slices     int
	Operations int
	Schemas    int
}

// Build fetches the index, every API reference page, extracts the embedded
// OpenAPI slices and merges them. The documentation is built from more than
// one OpenAPI file (each fence names its source, e.g. "/openapi.json" or
// "/preview/openapi.json"), so slices are merged per source and the result is
// keyed by source. Any page without a slice, any slice that does not contain
// the operation its fence header names, and any conflicting duplicate
// definition within one source is an error.
func Build(ctx context.Context, f *Fetcher, indexURL string) (map[string]map[string]any, map[string]Stats, error) {
	index, err := f.Get(ctx, indexURL)
	if err != nil {
		return nil, nil, fmt.Errorf("fetching index: %w", err)
	}
	pages, err := ReferencePages(index, indexURL)
	if err != nil {
		return nil, nil, err
	}
	if len(pages) == 0 {
		return nil, nil, fmt.Errorf("no API reference pages found in %s", indexURL)
	}
	f.logf("found %d API reference pages", len(pages))

	bodies := make([][]byte, len(pages))
	errs := make([]error, len(pages))
	conc := f.Concurrency
	if conc <= 0 {
		conc = 4
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, p := range pages {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			bodies[i], errs[i] = f.Get(ctx, p)
		}(i, p)
	}
	wg.Wait()
	var fetchErrs []string
	for i, err := range errs {
		if err != nil {
			fetchErrs = append(fetchErrs, fmt.Sprintf("%s: %v", pages[i], err))
		}
	}
	if len(fetchErrs) > 0 {
		return nil, nil, fmt.Errorf("failed to fetch %d page(s):\n  %s", len(fetchErrs), strings.Join(fetchErrs, "\n  "))
	}

	ms := NewMultiMerger()
	// Pages are processed in sorted URL order so conflict reports and
	// first-origin bookkeeping are deterministic.
	for i, p := range pages {
		if err := ms.AddPage(p, bodies[i]); err != nil {
			return nil, nil, err
		}
	}
	return ms.Result()
}

// MultiMerger merges slices into one document per source OpenAPI file.
type MultiMerger struct {
	bySource map[string]*Merger
	pages    map[string]int
}

// NewMultiMerger returns an empty MultiMerger.
func NewMultiMerger() *MultiMerger {
	return &MultiMerger{bySource: map[string]*Merger{}, pages: map[string]int{}}
}

// Result returns the merged document and statistics for every source, or an
// error listing all conflicts in all sources.
func (ms *MultiMerger) Result() (map[string]map[string]any, map[string]Stats, error) {
	docs := map[string]map[string]any{}
	stats := map[string]Stats{}
	var all []Conflict
	for _, src := range sortedKeys(ms.bySource) {
		m := ms.bySource[src]
		doc, err := m.Result()
		if err != nil {
			var ce *ConflictError
			if errors.As(err, &ce) {
				for _, c := range ce.Conflicts {
					c.Pointer = src + "#" + c.Pointer
					all = append(all, c)
				}
				continue
			}
			return nil, nil, err
		}
		docs[src] = doc
		stats[src] = computeStats(doc, ms.pages[src], m.Slices())
	}
	if len(all) > 0 {
		return nil, nil, &ConflictError{Conflicts: all}
	}
	return docs, stats, nil
}

func computeStats(doc map[string]any, pages, slices int) Stats {
	st := Stats{Pages: pages, Slices: slices}
	if paths, ok := doc["paths"].(map[string]any); ok {
		for _, item := range paths {
			if im, ok := item.(map[string]any); ok {
				for k := range im {
					if isMethod(k) {
						st.Operations++
					}
				}
			}
		}
	}
	if comps, ok := doc["components"].(map[string]any); ok {
		if s, ok := comps["schemas"].(map[string]any); ok {
			st.Schemas = len(s)
		}
	}
	return st
}

// AddPage extracts every OpenAPI slice in one markdown page and merges each
// into the document for its source file.
func (ms *MultiMerger) AddPage(pageURL string, body []byte) error {
	blocks, err := ExtractBlocks(body)
	if err != nil {
		return fmt.Errorf("%s: %w", pageURL, err)
	}
	if len(blocks) == 0 {
		return fmt.Errorf("%s: no embedded OpenAPI document found (has the docs page format changed?)", pageURL)
	}
	seen := map[string]bool{}
	for _, b := range blocks {
		if !validSource.MatchString(b.Source) {
			return fmt.Errorf("%s: unexpected OpenAPI source %q in fence header", pageURL, b.Source)
		}
		v, err := YAMLToJSON(b.YAML)
		if err != nil {
			return fmt.Errorf("%s: parsing %s %s: %w", pageURL, b.Method, b.Path, err)
		}
		doc, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: embedded document is not an object", pageURL)
		}
		if ver, _ := doc["openapi"].(string); !strings.HasPrefix(ver, "3.") {
			return fmt.Errorf("%s: unexpected openapi version %q", pageURL, ver)
		}
		paths, _ := doc["paths"].(map[string]any)
		item, _ := paths[b.Path].(map[string]any)
		if _, ok := item[b.Method]; !ok {
			return fmt.Errorf("%s: fence names %s %s but the document does not define it", pageURL, b.Method, b.Path)
		}
		m := ms.bySource[b.Source]
		if m == nil {
			m = NewMerger()
			ms.bySource[b.Source] = m
		}
		if err := m.Add(pageURL, doc); err != nil {
			return err
		}
		seen[b.Source] = true
	}
	for src := range seen {
		ms.pages[src]++
	}
	return nil
}

func isMethod(k string) bool {
	switch k {
	case "get", "put", "post", "delete", "options", "head", "patch", "trace":
		return true
	}
	return false
}
