// Package main provides small, deterministic helpers for the agent shell
// entrypoint. Keep this binary narrow: it exists to move fragile text/time
// parsing out of bash without rewriting the whole runtime at once.
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var reasoningBlock = regexp.MustCompile(`(?is)<(think|thinking|reasoning)>.*?</(think|thinking|reasoning)>\s*`)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "strip-reasoning":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Print(strings.TrimSpace(reasoningBlock.ReplaceAllString(string(data), "")))
	case "now-seconds":
		fmt.Println(time.Now().Unix())
	case "now-ms":
		fmt.Println(time.Now().UnixMilli())
	case "duration-seconds":
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "duration-seconds requires start unix seconds")
			os.Exit(2)
		}
		start, err := strconv.ParseInt(os.Args[2], 10, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println(time.Now().Unix() - start)
	case "build-user-content":
		if err := buildUserContent(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: vornik-agent-helper strip-reasoning|now-seconds|now-ms|duration-seconds <start>|build-user-content [flags]")
}

// imageContent is the OpenAI Chat Completions multimodal content block
// for an inline image — kept local so this helper has zero dependencies
// on the daemon's chat package and can be cross-compiled into the agent
// image without dragging in the larger module graph.
type imageContent struct {
	URL string `json:"url"`
}

type contentBlock struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *imageContent `json:"image_url,omitempty"`
}

// buildUserContent emits the JSON value for the `content` field of a
// user message. When no images are attached the output is a JSON string
// (the fast path that every existing caller in the swarm relies on).
// When one or more images are attached the output is a JSON array of
// content blocks: a leading text block carrying the prompt, followed by
// one image_url block per attached image.
//
// Image bytes are inlined as data: URLs with a base64 payload; the MIME
// type is inferred from the file extension. Unsupported extensions and
// missing files are reported to stderr and skipped — better to send a
// degraded text-only request than to crash the entire agent step.
func buildUserContent(args []string) error {
	fs := flag.NewFlagSet("build-user-content", flag.ContinueOnError)
	textFile := fs.String("text-file", "", "path to a file containing the prompt text (use '-' for stdin)")
	text := fs.String("text", "", "literal prompt text (alternative to --text-file)")
	var images stringList
	fs.Var(&images, "image", "path to an image file (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var promptText string
	switch {
	case *textFile == "-":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		promptText = string(data)
	case *textFile != "":
		data, err := os.ReadFile(*textFile)
		if err != nil {
			return fmt.Errorf("read %s: %w", *textFile, err)
		}
		promptText = string(data)
	default:
		promptText = *text
	}

	// No images → emit the prompt as a JSON string. Existing callers
	// (and the chat-layer string fast path) depend on this shape.
	if len(images) == 0 {
		out, err := json.Marshal(promptText)
		if err != nil {
			return fmt.Errorf("marshal text: %w", err)
		}
		_, err = os.Stdout.Write(out)
		return err
	}

	blocks := make([]contentBlock, 0, len(images)+1)
	if promptText != "" {
		blocks = append(blocks, contentBlock{Type: "text", Text: promptText})
	}
	for _, p := range images {
		mime, err := mimeFromExt(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "build-user-content: skipping %s: %v\n", p, err)
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "build-user-content: skipping %s: %v\n", p, err)
			continue
		}
		url := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
		blocks = append(blocks, contentBlock{Type: "image_url", ImageURL: &imageContent{URL: url}})
	}

	// All images failed to load → fall back to the text-only fast path
	// so the chat call still succeeds, just without vision.
	if len(blocks) == 0 || (len(blocks) == 1 && blocks[0].Type == "text") {
		if len(blocks) == 0 {
			blocks = []contentBlock{{Type: "text", Text: promptText}}
		}
		out, err := json.Marshal(blocks[0].Text)
		if err != nil {
			return fmt.Errorf("marshal text fallback: %w", err)
		}
		_, err = os.Stdout.Write(out)
		return err
	}

	out, err := json.Marshal(blocks)
	if err != nil {
		return fmt.Errorf("marshal blocks: %w", err)
	}
	_, err = os.Stdout.Write(out)
	return err
}

// mimeFromExt maps a file extension to the corresponding image MIME
// type. Only the formats supported by the major vision providers
// (Gemini, Claude, GPT-4o) are accepted; anything else is rejected so
// the caller doesn't smuggle a non-image file into the prompt.
func mimeFromExt(path string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg", nil
	case ".png":
		return "image/png", nil
	case ".gif":
		return "image/gif", nil
	case ".webp":
		return "image/webp", nil
	default:
		return "", fmt.Errorf("unsupported image extension %q (want .jpg/.jpeg/.png/.gif/.webp)", ext)
	}
}

// stringList is a flag.Value backing for repeatable string flags.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}
