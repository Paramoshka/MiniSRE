package agent

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
)

// ErrContextLengthExceeded means the provider rejected a request that does not
// fit its context window. It does not describe a truncated generated answer.
var ErrContextLengthExceeded = errors.New("agent: context length exceeded")

func classifyError(err error) error {
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) || (apiErr.StatusCode != http.StatusBadRequest && apiErr.StatusCode != http.StatusUnprocessableEntity) {
		return err
	}
	message := strings.ToLower(apiErr.Message)
	if apiErr.Code == "context_length_exceeded" ||
		(strings.Contains(message, "maximum context length") &&
			(strings.Contains(message, "requested") || strings.Contains(message, "exceed"))) {
		return fmt.Errorf("%w: %w", ErrContextLengthExceeded, err)
	}
	return err
}
