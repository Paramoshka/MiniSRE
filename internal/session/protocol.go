package session

import (
	"errors"
	"regexp"
	"strings"
)

// ErrResponseFormat indicates that the model could not produce a valid final answer.
var ErrResponseFormat = errors.New("agent: invalid model response format")

var toolMarkup = regexp.MustCompile(`<[/]?[｜|]{1,2}DSML[｜|]{1,2}\s*(calls|tool_calls|invoke)\b`)

// Ignore Markdown examples; executable-looking DSML must arrive as tool_calls.
func hasToolMarkup(content string) bool {
	fence := ""
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, ">") {
			continue
		}
		if fence != "" {
			if strings.HasPrefix(line, fence) && strings.Trim(line, fence[:1]+" \t") == "" {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			fence = line[:len(line)-len(strings.TrimLeft(line, line[:1]))]
			continue
		}
		// Inline code can use multiple backticks to quote literal backticks.
		for start := strings.IndexByte(line, '`'); start >= 0; start = strings.IndexByte(line, '`') {
			rest := line[start:]
			width := len(rest) - len(strings.TrimLeft(rest, "`"))
			end := strings.Index(rest[width:], rest[:width])
			if end < 0 {
				break
			}
			line = line[:start] + rest[width+end+width:]
		}
		if toolMarkup.MatchString(line) {
			return true
		}
	}
	return false
}
