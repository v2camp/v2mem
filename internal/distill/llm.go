package distill

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
)

func completeLLM(ctx context.Context, spec ModelSpec, prompt string) (string, error) {
	switch spec.Kind {
	case "ollama":
		return completeOllama(ctx, spec.Name, prompt)
	case "openai":
		return completeOpenAI(ctx, spec, prompt)
	default:
		return "", fmt.Errorf("未知模型种类: %s", spec.Kind)
	}
}

func completeOllama(ctx context.Context, model, prompt string) (string, error) {
	cmd := exec.CommandContext(ctx, "ollama", "run", model)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("ollama: %s", msg)
	}
	return stdout.String(), nil
}

func completeOpenAI(ctx context.Context, spec ModelSpec, prompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model": spec.Name,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0,
	})
	if err != nil {
		return "", err
	}
	base := strings.TrimRight(spec.BaseURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+spec.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("openai http %d", resp.StatusCode)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("openai JSON 不合规: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("openai 没有 choices")
	}
	return parsed.Choices[0].Message.Content, nil
}
