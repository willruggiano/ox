package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	perplexityEndpoint  = "https://api.perplexity.ai/v1/agent"
	perplexityAPIKeyEnv = "PERPLEXITY_API_KEY"

	// scoutDefaultModel routes through Perplexity's Agent API to Anthropic's
	// Opus 4.7. Any other model ID accepted by /v1/agent works too — see
	// https://docs.perplexity.ai/docs/agent-api/models.
	scoutDefaultModel = "anthropic/claude-opus-4-7"

	scoutDefaultQuestion = "Scout the world for prior art, people, and recent developments relevant to the substance of this conversation."

	scoutSystemPrompt = `You are a scout for a working software team. You read a slice of their meeting transcript and surface a few outside findings that would make them stop and say "that's absolutely brilliant."

First, silently identify the 2-4 non-obvious things the team is actually wrestling with: the contested assumptions, the architectural decisions they're debating, the thing they're building by hand, the mental model they're reaching for. Extract rare nouns, unresolved decisions, operational anxieties, named devices/protocols/regulations, and phrases implying "we are about to build this ourselves." Target these — NOT the brand names or generic category nouns in the transcript.

Then search the web based on those things and based on the results, surface the single most important finding. The finding must be something a sharp engineer on this team probably does NOT already know, specific enough to search by name, and additive (it accelerates them — never criticizes work they've done).

Cover recent information specifically: something that changed in roughly the last 6 weeks — a release, launch, paper, talk, deprecation, or acquisition that moves the goalposts. The finding MUST state the month or quarter inline (e.g. "March 2026", "Q1 2026").

If no genuinely strong recent finding exists for this slice, return nothing rather than pad with weak or fabricated results.

The finding must name a concrete, Googleable referent: a repo name, a paper title, a blog-post title, a company + specific feature/doc, or a person + their talk. Do not return a list of well-known SaaS products.

Household-name tools the team obviously already knows are disqualified unless paired with a specific non-obvious technique or a recent change to that tool.

Output rules (hard):
- the teams time is worth $1 million dollars per minute per member.  Don't waste it.  Make the response short.
- A single bullet. Nothing else.
- At most 3 sentences: one opener naming the thing, one specific detail, and 1 EXTREMELY SHORT reason to pay attention.
- Lead the bullet with the bolded name of the referent.
- No preamble, no intro line, no restating what the meeting was about, no closing summary, no section headers.
- Plain markdown bullets only.`
)

type scoutArgs struct {
	transcriptPath string
	question       string
	model          string
	domains        []string
	showCitations  bool
	jsonOnly       bool
}

var scoutCmd = &cobra.Command{
	Use:   "scout <transcript-path>",
	Short: "Search the web for prior art related to a meeting",
	Long: `Read a meeting transcript, then ask Perplexity's Agent API for prior art,
people, and recent developments relevant to what the team discussed.

Pass "-" as the path to read the transcript from stdin.

Requires the PERPLEXITY_API_KEY environment variable.

Examples:
  ox scout ./meeting.md
  cat transcript.txt | ox scout -
  ox scout ./meeting.md --question "What open-source libraries solve this?"
  ox scout ./meeting.md --model anthropic/claude-sonnet-4-6
  ox scout ./meeting.md --json`,
	Args: cobra.ExactArgs(1),
	RunE: runScout,
}

func init() {
	scoutCmd.Flags().String("question", "", "override the default research question")
	scoutCmd.Flags().String("model", scoutDefaultModel, "Perplexity Agent API model ID (see https://docs.perplexity.ai/docs/agent-api/models)")
	scoutCmd.Flags().StringSlice("domains", nil, "comma-separated search domain whitelist (default: no filter, full web)")
	// Citations are hidden by default: Perplexity's web search returns every
	// page it visited while researching, not only the ones the model actually
	// used in its answer. The model's inline named references — by exact
	// project/product name — are the real signal. Enable --citations to see
	// the raw list the agent searched.
	scoutCmd.Flags().Bool("citations", false, "show the raw 'Sources' list returned by Perplexity's web search")
	scoutCmd.Flags().Bool("json", false, "emit the raw Perplexity Agent API response as JSON")
}

// runScout is the cobra RunE for `ox scout`. It reads flags, builds a
// scoutArgs, validates required inputs (filling in the default question when
// none was given), and hands off to executeScout against real stdin/stdout.
func runScout(cmd *cobra.Command, args []string) error {
	question, _ := cmd.Flags().GetString("question")
	model, _ := cmd.Flags().GetString("model")
	domains, _ := cmd.Flags().GetStringSlice("domains")
	showCitations, _ := cmd.Flags().GetBool("citations")
	jsonOut, _ := cmd.Flags().GetBool("json")

	sa := &scoutArgs{
		transcriptPath: strings.TrimSpace(args[0]),
		question:       strings.TrimSpace(question),
		model:          strings.TrimSpace(model),
		domains:        domains,
		showCitations:  showCitations,
		jsonOnly:       jsonOut,
	}

	if sa.transcriptPath == "" {
		return fmt.Errorf("transcript path is required (use \"-\" for stdin)")
	}
	if sa.model == "" {
		return fmt.Errorf("--model is required")
	}
	if sa.question == "" {
		sa.question = scoutDefaultQuestion
	}

	return executeScout(sa, os.Stdin, os.Stdout)
}

// executeScout reads the transcript, calls Perplexity, and writes results to out.
// stdin is parameterized so tests can inject content for the "-" path.
func executeScout(sa *scoutArgs, stdin io.Reader, out io.Writer) error {
	apiKey := strings.TrimSpace(os.Getenv(perplexityAPIKeyEnv))
	if apiKey == "" {
		return fmt.Errorf("missing %s environment variable. Get a key at https://www.perplexity.ai/settings/api", perplexityAPIKeyEnv)
	}

	transcript, err := readTranscript(sa.transcriptPath, stdin)
	if err != nil {
		return err
	}
	if strings.TrimSpace(transcript) == "" {
		return fmt.Errorf("transcript is empty")
	}

	userInput := fmt.Sprintf("%s\n\n--- CONTEXT ---\n%s", sa.question, transcript)

	tool := perplexityTool{Type: "web_search"}
	if len(sa.domains) > 0 {
		tool.Filters = &perplexityToolFilters{SearchDomainFilter: sa.domains}
	}

	reqBody := perplexityAgentRequest{
		Model:        sa.model,
		Input:        userInput,
		Instructions: scoutSystemPrompt,
		Tools:        []perplexityTool{tool},
	}

	resp, raw, err := callPerplexity(apiKey, reqBody)
	if err != nil {
		return err
	}

	if sa.jsonOnly {
		_, err := out.Write(raw)
		return err
	}

	return renderScoutResponse(out, resp, sa.showCitations)
}

// readTranscript returns the transcript text for the given path, reading from
// stdin when path is "-". Read errors are wrapped with the source for context.
func readTranscript(path string, stdin io.Reader) (string, error) {
	if path == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(b), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read transcript %q: %w", path, err)
	}
	return string(b), nil
}

type perplexityAgentRequest struct {
	Model        string           `json:"model"`
	Input        string           `json:"input"`
	Instructions string           `json:"instructions,omitempty"`
	Tools        []perplexityTool `json:"tools,omitempty"`
}

type perplexityTool struct {
	Type    string                 `json:"type"`
	Filters *perplexityToolFilters `json:"filters,omitempty"`
}

type perplexityToolFilters struct {
	SearchDomainFilter []string `json:"search_domain_filter,omitempty"`
}

type perplexityAgentResponse struct {
	ID     string                 `json:"id"`
	Model  string                 `json:"model"`
	Status string                 `json:"status"`
	Output []perplexityOutputItem `json:"output"`
}

type perplexityOutputItem struct {
	Type    string                     `json:"type"`
	Role    string                     `json:"role,omitempty"`
	Content []perplexityMessageContent `json:"content,omitempty"`
	// search_results-only fields
	Queries []string                 `json:"queries,omitempty"`
	Results []perplexitySearchResult `json:"results,omitempty"`
}

type perplexityMessageContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type perplexitySearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
	Date    string `json:"date,omitempty"`
}

// callPerplexity POSTs the request to Perplexity's Agent API and returns the
// decoded response alongside the raw body (so callers can emit --json without a
// re-marshal). A non-2xx status yields an error carrying a truncated body
// excerpt; the raw bytes are still returned for diagnostics.
func callPerplexity(apiKey string, body perplexityAgentRequest) (*perplexityAgentResponse, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, perplexityEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// 120s budget: the agent endpoint runs one or more web-search tool calls,
	// then has the chosen model synthesize an answer. Opus + multi-query
	// search routinely takes 30-60s; sonar-pro alone returns in under 15s.
	client := &http.Client{Timeout: 120 * time.Second}
	httpResp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("perplexity request failed: %w", err)
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read perplexity response: %w", err)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		excerpt := string(raw)
		if len(excerpt) > 500 {
			excerpt = excerpt[:500] + "..."
		}
		return nil, raw, fmt.Errorf("perplexity returned %s: %s", httpResp.Status, excerpt)
	}

	var resp perplexityAgentResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, raw, fmt.Errorf("decode perplexity response: %w", err)
	}
	return &resp, raw, nil
}

// renderScoutResponse writes the agent's message text to out, and — when
// showCitations is set and the response carried search results — appends the
// raw "Sources" list. It errors if the response contained no message content.
func renderScoutResponse(out io.Writer, resp *perplexityAgentResponse, showCitations bool) error {
	var messageText strings.Builder
	var searchResults []perplexitySearchResult
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				// "output_text" is what Opus returns; "text" is a defensive
				// fallback in case another model surfaces content under that key.
				if c.Type == "output_text" || c.Type == "text" {
					messageText.WriteString(c.Text)
				}
			}
		case "search_results":
			searchResults = append(searchResults, item.Results...)
		}
	}

	text := strings.TrimSpace(messageText.String())
	if text == "" {
		return fmt.Errorf("perplexity returned no message content")
	}
	if _, err := fmt.Fprintln(out, text); err != nil {
		return err
	}

	if showCitations && len(searchResults) > 0 {
		if _, err := fmt.Fprintln(out, "\nSources:"); err != nil {
			return err
		}
		for i, r := range searchResults {
			if _, err := fmt.Fprintf(out, "  [%d] %s — %s\n", i+1, r.Title, r.URL); err != nil {
				return err
			}
		}
	}
	return nil
}
