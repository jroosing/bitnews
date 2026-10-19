package adaptertest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Fixture is one recorded (or hand-written) exchange, or a short sequence of
// them that share captured values.
type Fixture struct {
	Name string `json:"name"`
	// Provenance says where the fixture comes from, such as "recorded from
	// Sonarr 4.0.9 against SABnzbd 4.3.1" or "hand-written from the docs,
	// NOT recorded". It is required, so that a reader of the tests always
	// knows how much to trust them.
	Provenance  string `json:"provenance"`
	Description string `json:"description,omitempty"`
	// Seed describes the state to create before the first step. This
	// package does not interpret it.
	Seed  []json.RawMessage `json:"seed,omitempty"`
	Steps []Step            `json:"steps"`

	// Path is the file the fixture was loaded from. Dir is where its
	// multipart "file" parts are read from.
	Path string `json:"-"`
	Dir  string `json:"-"`
}

// Step is one request and the response it must get.
type Step struct {
	Name     string   `json:"name,omitempty"`
	Request  Request  `json:"request"`
	Response Response `json:"response"`
	// Capture stores values of the response as variables for the next
	// steps: name to a path like "$.nzo_ids[0]".
	Capture map[string]string `json:"capture,omitempty"`
}

// Request is the HTTP request to send.
type Request struct {
	Method string `json:"method"`
	// Target is the path and query.
	Target  string            `json:"target"`
	Headers map[string]string `json:"headers,omitempty"`
	// Body is sent as is, with the headers' Content-Type. Use Multipart
	// for an upload.
	Body      string `json:"body,omitempty"`
	Multipart []Part `json:"multipart,omitempty"`
}

// Part is one part of a multipart/form-data body.
type Part struct {
	Name        string `json:"name"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	// Text is the part's content; File names a file in the fixture
	// directory's files/ folder instead.
	Text string `json:"text,omitempty"`
	File string `json:"file,omitempty"`
}

// Response is what the reply must look like.
type Response struct {
	Status int `json:"status"`
	// Headers maps header names to a value or matcher.
	Headers map[string]string `json:"headers,omitempty"`
	// JSON is the expected JSON body with matchers; absent means the body
	// is not checked as JSON.
	JSON json.RawMessage `json:"json,omitempty"`
	// Body is the expected plain-text body, an exact string or "$re:..."
	// matcher.
	Body *string `json:"body,omitempty"`
}

// Load reads every *.json file of dir as a Fixture, in file name order. A
// file that does not decode strictly, or lacks a name, provenance, request
// or status, is an error.
func Load(dir string) ([]*Fixture, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	slices.Sort(paths)
	out := make([]*Fixture, 0, len(paths))
	for _, p := range paths {
		fx, err := LoadFile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, fx)
	}
	return out, nil
}

// LoadFile reads one fixture.
func LoadFile(path string) (*Fixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var fx Fixture
	if err := dec.Decode(&fx); err != nil {
		return nil, fmt.Errorf("adaptertest: %s: %w", path, err)
	}
	fx.Path, fx.Dir = path, filepath.Join(filepath.Dir(path), "files")
	if err := fx.validate(); err != nil {
		return nil, fmt.Errorf("adaptertest: %s: %w", path, err)
	}
	return &fx, nil
}

func (fx *Fixture) validate() error {
	var problems []error
	if strings.TrimSpace(fx.Name) == "" {
		problems = append(problems, errors.New("no name"))
	}
	if strings.TrimSpace(fx.Provenance) == "" {
		problems = append(problems, errors.New("no provenance"))
	}
	if len(fx.Steps) == 0 {
		problems = append(problems, errors.New("no steps"))
	}
	for i, s := range fx.Steps {
		if s.Request.Method == "" || !strings.HasPrefix(s.Request.Target, "/") {
			problems = append(problems, fmt.Errorf("step %d: request needs a method and a target starting with /", i+1))
		}
		if s.Response.Status == 0 {
			problems = append(problems, fmt.Errorf("step %d: response needs a status", i+1))
		}
	}
	return errors.Join(problems...)
}
