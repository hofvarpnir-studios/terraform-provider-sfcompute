// Package specmerge reconstructs SF Compute's OpenAPI document from the
// per-endpoint slices that their Mintlify documentation embeds in every API
// reference page.
//
// Each page (for example
// https://docs.sfcompute.com/api-reference/orders/create-order.md) contains a
// fenced block that starts with a line such as
//
//	````yaml /openapi.json post /v2/orders
//
// followed by a complete `openapi: 3.1.0` document that holds that one
// operation plus every component it references, and ends with a line that is
// just "````". This package extracts those blocks, converts them to plain JSON
// values and merges them into one deterministic document.
package specmerge

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

// Block is one embedded OpenAPI document slice.
type Block struct {
	// Source is the spec file Mintlify built the page from, for example
	// "/openapi.json" or "/preview/openapi.json".
	Source string
	// Method is the lower-case HTTP method of the documented operation.
	Method string
	// Path is the documented route, for example "/v2/orders".
	Path string
	// YAML is the raw document text between the fences.
	YAML []byte
}

const fence = "````"

// ExtractBlocks returns every "````yaml <source> <method> <path>" block in a
// Mintlify markdown page. An unterminated block is an error.
func ExtractBlocks(markdown []byte) ([]Block, error) {
	var (
		blocks []Block
		cur    *Block
		buf    bytes.Buffer
		lineNo int
	)
	sc := bufio.NewScanner(bytes.NewReader(markdown))
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if cur == nil {
			if !strings.HasPrefix(line, fence+"yaml") {
				continue
			}
			fields := strings.Fields(strings.TrimPrefix(line, fence+"yaml"))
			if len(fields) != 3 {
				// Some other yaml block (for example an example payload);
				// only the OpenAPI slices carry "<source> <method> <path>".
				continue
			}
			cur = &Block{Source: fields[0], Method: strings.ToLower(fields[1]), Path: fields[2]}
			buf.Reset()
			continue
		}
		if strings.TrimRight(line, " \t") == fence {
			cur.YAML = append([]byte(nil), buf.Bytes()...)
			blocks = append(blocks, *cur)
			cur = nil
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if cur != nil {
		return nil, fmt.Errorf("unterminated %s %s %s block (reached line %d)", cur.Source, cur.Method, cur.Path, lineNo)
	}
	return blocks, nil
}
