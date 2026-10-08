package provider

// Amazon Bedrock Converse-Stream client.
//
// Endpoint: POST https://bedrock-runtime.{region}.amazonaws.com/model/{id}/converse-stream
//
// Auth: two paths supported.
//
//   - **Bearer token** via AWS_BEARER_TOKEN_BEDROCK. Modern, simple,
//     `bedrock:CallWithBearerToken` IAM permission required.
//   - **SigV4 signing** via AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY
//     (+ optional AWS_SESSION_TOKEN for temporary credentials, e.g. SSO
//     or IRSA-issued). Hand-rolled here so we don't pull in aws-sdk-go-v2.
//
// If both are present, bearer wins (simpler / fewer moving parts).
//
// Wire format: AWS event-stream binary framing. Each message is:
//
//	[4-byte total length BE]
//	[4-byte headers length BE]
//	[4-byte CRC32 of the 8-byte prelude]
//	[headers ...]      each = [1B name-length][name][1B value-type][2B value-length][value]
//	[payload ...]      payload-length = total - headers - 4 (trailer) - 12 (prelude+prelude-crc)
//	[4-byte CRC32 of message bytes minus trailer]
//
// We only consume the stream — never produce it — so we don't validate
// CRCs (the TLS+TCP transport already protects in-flight bytes). We do
// validate length-bounds to avoid OOB reads on a hostile / corrupted
// stream.
//
// Event types we care about (from `:event-type` header):
//   - messageStart        { role: "assistant" }
//   - contentBlockStart   { contentBlockIndex, start: { toolUse: { toolUseId, name } } }
//   - contentBlockDelta   { contentBlockIndex, delta: { text?, toolUse?: { input } } }
//   - contentBlockStop    { contentBlockIndex }
//   - messageStop         { stopReason }
//   - metadata            { usage: { inputTokens, outputTokens } }

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

type bedrockClient struct {
	// Auth mode is determined at construction time. Exactly one of
	// bearerToken or sigv4 is populated.
	bearerToken string
	sigv4       *bedrockSigV4Creds

	region  string
	baseURL string
	http    *http.Client
}

// bedrockSigV4Creds holds the IAM credentials used to sign each request.
type bedrockSigV4Creds struct {
	accessKeyID     string
	secretAccessKey string
	sessionToken    string // optional; STS / SSO / IRSA temp creds
}

// NewBedrockClient returns a Bedrock client.
//
// Auth resolution (first match wins):
//
//  1. apiKey == real bearer-ish string (not "<aws>") -> bearer route.
//  2. AWS_BEARER_TOKEN_BEDROCK env var -> bearer route.
//  3. AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY (+ optional
//     AWS_SESSION_TOKEN) -> SigV4 route.
//  4. AWS_PROFILE -> read ~/.aws/credentials, take that profile's keys.
//  5. AWS_PROFILE -> `aws configure export-credentials` (SSO, assume-role,
//     credential_process) when the profile has no static keys.
//
// region defaults to us-east-1 unless AWS_REGION / AWS_DEFAULT_REGION is
// set or the baseURL embeds a region.
func NewBedrockClient(apiKey, baseURL string) Client {
	region := bedrockResolveRegion()
	if baseURL == "" {
		baseURL = "https://bedrock-runtime." + region + ".amazonaws.com"
	}
	c := &bedrockClient{
		region:  region,
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 0},
	}

	bearer, sigv4 := resolveBedrockAuth(apiKey)
	if bearer != "" {
		c.bearerToken = bearer
		return c
	}
	if sigv4 != nil {
		c.sigv4 = sigv4
		return c
	}

	return &unimplementedClient{
		name: "amazon-bedrock",
		hint: "no Bedrock credentials found (set AWS_BEARER_TOKEN_BEDROCK, AWS_ACCESS_KEY_ID+AWS_SECRET_ACCESS_KEY, or AWS_PROFILE)",
	}
}

// bedrockResolveRegion returns the AWS region for Bedrock calls,
// honoring AWS_REGION then AWS_DEFAULT_REGION, defaulting to us-east-1.
func bedrockResolveRegion() string {
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if region == "" {
		region = "us-east-1"
	}
	return region
}

// resolveBedrockAuth determines Bedrock credentials from the same
// sources NewBedrockClient uses, so discovery and inference share one
// resolution path. Exactly one of (bearer, sigv4) is non-empty on
// success; both are zero when no credentials are found.
//
// Resolution order (first match wins): explicit apiKey bearer, then
// AWS_BEARER_TOKEN_BEDROCK, then AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY
// env vars, then ~/.aws/credentials via AWS_PROFILE.
func resolveBedrockAuth(apiKey string) (bearer string, sigv4 *bedrockSigV4Creds) {
	token := apiKey
	if token == "" || token == "<aws>" {
		token = os.Getenv("AWS_BEARER_TOKEN_BEDROCK")
	}
	if token != "" && token != "<aws>" {
		return token, nil
	}

	return "", resolveBedrockDiscoveryCreds()
}

// resolveBedrockDiscoveryCreds ignores inference bearer tokens: Bedrock's
// control-plane listing endpoints require SigV4 even when inference uses a bearer.
func resolveBedrockDiscoveryCreds() *bedrockSigV4Creds {
	ak := os.Getenv("AWS_ACCESS_KEY_ID")
	sk := os.Getenv("AWS_SECRET_ACCESS_KEY")
	st := os.Getenv("AWS_SESSION_TOKEN")
	if ak != "" && sk != "" {
		return &bedrockSigV4Creds{accessKeyID: ak, secretAccessKey: sk, sessionToken: st}
	}

	if profile := os.Getenv("AWS_PROFILE"); profile != "" {
		if creds, err := readAWSCredentialsFile(profile); err == nil {
			return creds
		}
	}

	// Last resort: ask the AWS CLI to resolve credentials for the active
	// profile. This handles SSO, assumed roles, credential_process, and
	// web-identity setups whose credentials are not stored as static keys
	// in ~/.aws/credentials. Best-effort: absent CLI or an expired login
	// simply yields no credentials.
	if creds := resolveBedrockCredsViaCLI(); creds != nil {
		return creds
	}

	return nil
}

// resolveBedrockCredsViaCLI shells out to `aws configure
// export-credentials --format process`, which returns fully-resolved
// credentials for the active profile regardless of how that profile is
// configured (static keys, SSO, assume-role, credential_process). The
// AWS CLI must be installed and the profile's login (if any) still
// valid. Returns nil on any failure so the caller can fall through.
func resolveBedrockCredsViaCLI() *bedrockSigV4Creds {
	if _, err := exec.LookPath("aws"); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// AWS_PROFILE / AWS_REGION already in the environment are inherited,
	// so the CLI resolves the same profile zot was pointed at.
	cmd := exec.CommandContext(ctx, "aws", "configure", "export-credentials", "--format", "process")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var exported struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		SessionToken    string `json:"SessionToken"`
	}
	if err := json.Unmarshal(out, &exported); err != nil {
		return nil
	}
	if exported.AccessKeyID == "" || exported.SecretAccessKey == "" {
		return nil
	}
	return &bedrockSigV4Creds{
		accessKeyID:     exported.AccessKeyID,
		secretAccessKey: exported.SecretAccessKey,
		sessionToken:    exported.SessionToken,
	}
}

// readAWSCredentialsFile parses ~/.aws/credentials and returns the
// access-key/secret-key (and optional session-token) for the named
// profile. The file is an INI-like format with `[profile]` headers.
func readAWSCredentialsFile(profile string) (*bedrockSigV4Creds, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(home + "/.aws/credentials")
	if err != nil {
		return nil, err
	}
	var current string
	creds := map[string]map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.TrimSpace(line[1 : len(line)-1])
			creds[current] = map[string]string{}
			continue
		}
		if current == "" {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		creds[current][k] = v
	}
	p, ok := creds[profile]
	if !ok {
		return nil, fmt.Errorf("aws profile %q not found in ~/.aws/credentials", profile)
	}
	ak := p["aws_access_key_id"]
	sk := p["aws_secret_access_key"]
	if ak == "" || sk == "" {
		return nil, fmt.Errorf("aws profile %q missing aws_access_key_id or aws_secret_access_key", profile)
	}
	return &bedrockSigV4Creds{
		accessKeyID:     ak,
		secretAccessKey: sk,
		sessionToken:    p["aws_session_token"],
	}, nil
}

func (c *bedrockClient) Name() string { return "amazon-bedrock" }

// ---- request building ----

type bedrockMessage struct {
	Role    string                   `json:"role"`
	Content []map[string]interface{} `json:"content"`
}

// bedrockCachePoint is the Converse API cache checkpoint content block.
// Appending this to a system or message content array tells Bedrock to
// create a cache boundary at that position in the prompt prefix.
var bedrockCachePoint = map[string]interface{}{
	"cachePoint": map[string]interface{}{"type": "default"},
}

type bedrockToolSpec struct {
	ToolSpec struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		InputSchema struct {
			JSON json.RawMessage `json:"json"`
		} `json:"inputSchema"`
	} `json:"toolSpec"`
}

type bedrockRequest struct {
	Messages                     []bedrockMessage         `json:"messages"`
	System                       []map[string]interface{} `json:"system,omitempty"`
	AdditionalModelRequestFields map[string]interface{}   `json:"additionalModelRequestFields,omitempty"`
	InferenceConfig              struct {
		MaxTokens   int      `json:"maxTokens,omitempty"`
		Temperature *float32 `json:"temperature,omitempty"`
	} `json:"inferenceConfig,omitempty"`
	ToolConfig *struct {
		Tools []bedrockToolSpec `json:"tools"`
	} `json:"toolConfig,omitempty"`
}

func normalizeBedrockToolResults(msgs []Message) []Message {
	resultByID := map[string]ToolResultBlock{}
	for _, m := range msgs {
		for _, c := range m.Content {
			if tr, ok := c.(ToolResultBlock); ok {
				if _, exists := resultByID[tr.CallID]; !exists {
					resultByID[tr.CallID] = tr
				}
			}
		}
	}

	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		copy := m
		copy.Content = nil
		var toolCalls []ToolCallBlock
		for _, c := range m.Content {
			switch v := c.(type) {
			case ToolResultBlock:
				// Bedrock requires toolResult blocks immediately after the
				// assistant toolUse they answer. Reinsert them from the
				// assistant pass below instead of preserving their original
				// location, which may be separated by user text in active
				// sessions.
				continue
			case ToolCallBlock:
				toolCalls = append(toolCalls, v)
			}
			copy.Content = append(copy.Content, c)
		}
		if len(copy.Content) > 0 {
			out = append(out, copy)
		}
		if m.Role != RoleAssistant || len(toolCalls) == 0 {
			continue
		}

		results := make([]Content, 0, len(toolCalls))
		for _, tc := range toolCalls {
			if tr, ok := resultByID[tc.ID]; ok {
				results = append(results, tr)
				continue
			}
			results = append(results, ToolResultBlock{
				CallID:  tc.ID,
				IsError: true,
				Content: []Content{TextBlock{Text: "tool call did not complete before the next user message"}},
			})
		}
		out = append(out, Message{Role: RoleTool, Content: results, Time: m.Time})
	}
	return out
}

// bedrockModelSupportsCaching reports whether the resolved model ID
// supports explicit prompt caching via cachePoint markers on Bedrock.
// We use PriceCacheWrite > 0 as a proxy: every Bedrock-hosted Claude
// model with a write price in the catalog supports cachePoint markers.
// Nova models use automatic caching and don't need explicit markers.
func bedrockModelSupportsCaching(modelID string) bool {
	// Strip geo prefix (us./eu./apac./au./global.) before catalog lookup.
	for _, p := range bedrockGeoPrefixes {
		if strings.HasPrefix(modelID, p+".") {
			modelID = modelID[len(p)+1:]
			break
		}
	}
	if m, err := FindModel("amazon-bedrock", modelID); err == nil {
		return m.PriceCacheWrite > 0
	}
	// Unknown model: enable for Anthropic Claude families — cachePoint is
	// silently ignored by the API if the model doesn't support it.
	return strings.HasPrefix(modelID, "anthropic.claude-")
}

// bedrockMessagesHaveToolBlocks reports whether any message in msgs
// contains a toolUse or toolResult content block. Bedrock's Converse API
// rejects the request with HTTP 400 if those block types are present but
// toolConfig is absent.
func bedrockMessagesHaveToolBlocks(msgs []bedrockMessage) bool {
	for _, m := range msgs {
		for _, c := range m.Content {
			if _, ok := c["toolUse"]; ok {
				return true
			}
			if _, ok := c["toolResult"]; ok {
				return true
			}
		}
	}
	return false
}

func (c *bedrockClient) buildRequest(req Request) (*bedrockRequest, error) {
	out := &bedrockRequest{}

	// Resolve the model ID as it will appear on the wire so the caching
	// check operates on the same ID used for FindModel.
	resolvedModel := resolveBedrockInferenceProfileID(req.Model, c.region)
	if resolvedModel == req.Model && (req.Model == "anthropic.claude-sonnet-5-5" || req.Model == "anthropic.claude-haiku-5-5") {
		return nil, fmt.Errorf("no regional inference profile for %q in %q: select an explicit inference profile (global.%s enables worldwide routing)", req.Model, c.region, req.Model)
	}
	caching := bedrockModelSupportsCaching(resolvedModel)
	model := Model{ID: resolvedModel}
	if catalogModel, err := FindModel("amazon-bedrock", resolvedModel); err == nil {
		model = catalogModel
	}
	reasoning := ClampReasoningForModel(model, req.Reasoning)
	adaptive := usesAdaptiveThinking(model)

	if req.System != "" {
		sysBlock := map[string]interface{}{"text": req.System}
		if caching {
			// Append cachePoint after the system text so the stable system
			// prompt is cached as the first breakpoint.
			out.System = []map[string]interface{}{sysBlock, bedrockCachePoint}
		} else {
			out.System = []map[string]interface{}{sysBlock}
		}
	}
	if !adaptive {
		out.InferenceConfig.Temperature = req.Temperature
	}
	if adaptive && reasoning != "" {
		out.AdditionalModelRequestFields = map[string]interface{}{
			"thinking":      map[string]interface{}{"type": "adaptive"},
			"output_config": map[string]interface{}{"effort": AnthropicAdaptiveEffort(reasoning)},
		}
	}
	out.InferenceConfig.MaxTokens = req.MaxTokens
	if out.InferenceConfig.MaxTokens == 0 {
		out.InferenceConfig.MaxTokens = 4096
	}
	for _, m := range normalizeBedrockToolResults(req.Messages) {
		role := string(m.Role)
		if role == "tool" {
			role = "user"
		}
		bm := bedrockMessage{Role: role}
		for _, c := range m.Content {
			switch v := c.(type) {
			case TextBlock:
				bm.Content = append(bm.Content, map[string]interface{}{"text": v.Text})
			case ToolCallBlock:
				var input map[string]interface{}
				_ = json.Unmarshal(v.Arguments, &input)
				if input == nil {
					input = map[string]interface{}{}
				}
				bm.Content = append(bm.Content, map[string]interface{}{
					"toolUse": map[string]interface{}{
						"toolUseId": v.ID, "name": v.Name, "input": input,
					},
				})
			case ImageBlock:
				data, mime := anthShrinkImageBytesIfTooBig(v.Data, v.MimeType)
				// Bedrock Converse uses the bare format name (jpeg/png/gif/webp).
				imgFmt := strings.TrimPrefix(mime, "image/")
				bm.Content = append(bm.Content, map[string]interface{}{
					"image": map[string]interface{}{
						"format": imgFmt,
						"source": map[string]interface{}{
							"bytes": base64.StdEncoding.EncodeToString(data),
						},
					},
				})
			case ToolResultBlock:
				var resultContent []map[string]interface{}
				for _, inner := range v.Content {
					switch iv := inner.(type) {
					case TextBlock:
						resultContent = append(resultContent, map[string]interface{}{"text": iv.Text})
					case ImageBlock:
						data, mime := anthShrinkImageBytesIfTooBig(iv.Data, iv.MimeType)
						imgFmt := strings.TrimPrefix(mime, "image/")
						resultContent = append(resultContent, map[string]interface{}{
							"image": map[string]interface{}{
								"format": imgFmt,
								"source": map[string]interface{}{
									"bytes": base64.StdEncoding.EncodeToString(data),
								},
							},
						})
					}
				}
				status := "success"
				if v.IsError {
					status = "error"
				}
				bm.Content = append(bm.Content, map[string]interface{}{
					"toolResult": map[string]interface{}{
						"toolUseId": v.CallID,
						"content":   resultContent,
						"status":    status,
					},
				})
			}
		}
		if len(bm.Content) == 0 {
			continue
		}
		out.Messages = append(out.Messages, bm)
	}
	activeTools := activeToolDefinitions(req.Tools, req.Messages)
	if len(activeTools) > 0 {
		tc := struct {
			Tools []bedrockToolSpec `json:"tools"`
		}{}
		for _, t := range activeTools {
			var ts bedrockToolSpec
			ts.ToolSpec.Name = t.Name
			ts.ToolSpec.Description = t.Description
			ts.ToolSpec.InputSchema.JSON = t.Schema
			tc.Tools = append(tc.Tools, ts)
		}
		out.ToolConfig = &tc
	} else if bedrockMessagesHaveToolBlocks(out.Messages) {
		// Bedrock requires toolConfig to be present whenever the message
		// history contains toolUse or toolResult content blocks, even if
		// the current request does not intend to call any tools (e.g. the
		// /btw side-chat sends the frozen main transcript which may include
		// prior tool turns). Inject a stub tool so the API accepts the
		// request; it will never be invoked since no tool_use stop reason
		// can be triggered when the real tool list is empty.
		var stub bedrockToolSpec
		stub.ToolSpec.Name = "_placeholder"
		stub.ToolSpec.Description = "placeholder required by Bedrock when tool history is present"
		stub.ToolSpec.InputSchema.JSON = json.RawMessage(`{"type":"object","properties":{}}`)
		out.ToolConfig = &struct {
			Tools []bedrockToolSpec `json:"tools"`
		}{Tools: []bedrockToolSpec{stub}}
	}

	if caching {
		// Tag the last user message with a cachePoint. This extends the
		// cached prefix to cover the full conversation history up to the
		// current turn, so the next turn reads that history cheaply.
		bedrockTagLastUserCache(out.Messages)
	}

	return out, nil
}

// bedrockTagLastUserCache appends a cachePoint block to the last user
// message in the Bedrock message list. It is the Bedrock equivalent of
// Anthropic's cache_control:{type:"ephemeral"} on the last user message.
func bedrockTagLastUserCache(msgs []bedrockMessage) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			msgs[i].Content = append(msgs[i].Content, bedrockCachePoint)
			return
		}
	}
}

// resolveBedrockInferenceProfileID maps a bare foundation-model ID to
// its region-matched cross-region inference-profile ID.
//
// Several newer Bedrock models (Anthropic Claude 4.x, DeepSeek, etc.)
// cannot be invoked with on-demand throughput by their plain
// foundation-model ID; Bedrock returns HTTP 400 demanding "the ID or
// ARN of an inference profile that contains this model". The profile
// ID is the same model ID with a geographic prefix (us/eu/apac/...).
//
// We only rewrite IDs that (a) lack an existing geo prefix and (b)
// belong to a model family that requires a profile. IDs that already
// carry a prefix (e.g. "eu.anthropic...", "global.anthropic...") or
// fully-qualified ARNs are returned unchanged, so explicit user
// choices and custom application inference profiles still work.
func resolveBedrockInferenceProfileID(modelID, region string) string {
	if modelID == "" {
		return modelID
	}
	// ARNs are already inference-profile references; leave untouched.
	if strings.HasPrefix(modelID, "arn:") {
		return modelID
	}
	// Already geo-prefixed (us. / eu. / apac. / ap. / us-gov. / global.)?
	if bedrockHasGeoPrefix(modelID) {
		return modelID
	}
	// Opus 5 is exposed for on-demand throughput through the global
	// inference profile rather than a region-specific profile.
	if modelID == "anthropic.claude-opus-5" {
		return "global." + modelID
	}
	// These models have no APAC profile. Preserve geographic routing where
	// supported and require an explicit choice instead of falling back to global.
	if modelID == "anthropic.claude-sonnet-5-5" || modelID == "anthropic.claude-haiku-5-5" {
		prefix := bedrockGeoPrefixForRegion(region)
		if region == "" || strings.HasPrefix(region, "eu-") || strings.HasPrefix(region, "ca-") || (strings.HasPrefix(region, "us-") && !strings.HasPrefix(region, "us-gov-")) {
			return prefix + "." + modelID
		}
		if modelID == "anthropic.claude-haiku-5-5" {
			switch region {
			case "ap-southeast-2", "ap-southeast-4":
				return "au." + modelID
			case "ap-northeast-1", "ap-northeast-3":
				return "jp." + modelID
			}
		}
		return modelID
	}
	if !bedrockRequiresInferenceProfile(modelID) {
		return modelID
	}
	prefix := bedrockGeoPrefixForRegion(region)
	if prefix == "" {
		return modelID
	}
	return prefix + "." + modelID
}

// bedrockGeoPrefixes are the cross-region inference-profile geo
// prefixes Bedrock uses. A model ID that starts with one of these
// (followed by a dot) is already a profile reference.
var bedrockGeoPrefixes = []string{"us-gov", "us", "eu", "apac", "ap", "global", "au", "jp"}

func bedrockHasGeoPrefix(modelID string) bool {
	for _, p := range bedrockGeoPrefixes {
		if strings.HasPrefix(modelID, p+".") {
			return true
		}
	}
	return false
}

// bedrockRequiresInferenceProfile reports whether a bare
// foundation-model ID is one of the families AWS only exposes through
// a cross-region inference profile for on-demand throughput.
func bedrockRequiresInferenceProfile(modelID string) bool {
	switch {
	case strings.HasPrefix(modelID, "anthropic.claude-"):
		return true
	case strings.HasPrefix(modelID, "deepseek."):
		return true
	default:
		return false
	}
}

// bedrockGeoPrefixForRegion maps an AWS region to the geo prefix used
// by its cross-region inference profiles. Returns "" when the region
// has no known mapping, in which case the model ID is left unchanged.
func bedrockGeoPrefixForRegion(region string) string {
	switch {
	case region == "":
		return "us"
	case strings.HasPrefix(region, "us-gov-"):
		return "us-gov"
	case strings.HasPrefix(region, "us-"):
		return "us"
	case strings.HasPrefix(region, "eu-"):
		return "eu"
	case strings.HasPrefix(region, "ap-"):
		return "apac"
	case strings.HasPrefix(region, "ca-"):
		return "us"
	case strings.HasPrefix(region, "sa-"):
		return "us"
	default:
		return "us"
	}
}

func (c *bedrockClient) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	wire, err := c.buildRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	modelID := resolveBedrockInferenceProfileID(req.Model, c.region)
	url := c.baseURL + "/model/" + modelID + "/converse-stream"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "application/vnd.amazon.eventstream")
	if c.bearerToken != "" {
		httpReq.Header.Set("authorization", "Bearer "+c.bearerToken)
	} else if c.sigv4 != nil {
		if err := signSigV4(httpReq, body, "bedrock", c.region, c.sigv4, time.Now().UTC()); err != nil {
			return nil, fmt.Errorf("bedrock: sigv4 sign: %w", err)
		}
	} else {
		return nil, fmt.Errorf("bedrock: no auth configured")
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("bedrock: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		msg := strings.TrimSpace(string(b))
		// A 403 on the bearer route is almost always a region mismatch:
		// short-term Bedrock API keys are scoped to the region of the
		// console session that minted them, but zot defaults to
		// us-east-1. Surface the resolved region and the fix so the user
		// is not left guessing why a freshly-copied key is "invalid".
		if resp.StatusCode == http.StatusForbidden && c.bearerToken != "" {
			return nil, fmt.Errorf(
				"bedrock: http 403 (region=%s): %s\nhint: Bedrock API keys are region-scoped. If your key was created in another region, set AWS_REGION (e.g. AWS_REGION=eu-central-1) or pass --base-url https://bedrock-runtime.<region>.amazonaws.com",
				c.region, msg)
		}
		return nil, fmt.Errorf("bedrock: http %d: %s", resp.StatusCode, msg)
	}
	out := make(chan Event, 16)
	go c.runStream(ctx, resp, req, out)
	return out, nil
}

func (c *bedrockClient) runStream(ctx context.Context, resp *http.Response, req Request, out chan<- Event) {
	defer close(out)
	defer resp.Body.Close()

	out <- EventStart{Provider: "amazon-bedrock", Model: req.Model}

	// State accumulated across deltas.
	contentBlocks := map[int]*bedrockBlockState{}
	stop := StopEnd
	finalMsg := Message{Role: RoleAssistant, Time: time.Now()}
	var usage Usage

	for {
		if ctx.Err() != nil {
			stop = StopAborted
			break
		}
		evt, err := readEventStreamMessage(resp.Body)
		if err != nil {
			if err == io.EOF {
				break
			}
			out <- EventDone{Stop: StopError, Err: err, Message: finalMsg}
			return
		}
		eventType := evt.headerString(":event-type")
		if eventType == "" {
			eventType = bedrockEventTypeFromPayload(evt.payload)
		}
		messageType := evt.headerString(":message-type")
		if messageType == "exception" {
			out <- EventDone{Stop: StopError, Err: fmt.Errorf("bedrock exception (%s): %s", evt.headerString(":exception-type"), string(evt.payload)), Message: finalMsg}
			return
		}
		switch eventType {
		case "messageStart":
			// nothing to do; role is always "assistant"
		case "contentBlockStart":
			var d struct {
				ContentBlockIndex int `json:"contentBlockIndex"`
				Start             struct {
					ToolUse *struct {
						ToolUseID string `json:"toolUseId"`
						Name      string `json:"name"`
					} `json:"toolUse"`
				} `json:"start"`
			}
			if err := unmarshalBedrockEventPayload(evt.payload, "contentBlockStart", &d); err != nil {
				continue
			}
			st := &bedrockBlockState{}
			contentBlocks[d.ContentBlockIndex] = st
			if d.Start.ToolUse != nil {
				st.isToolUse = true
				st.toolID = d.Start.ToolUse.ToolUseID
				st.toolName = d.Start.ToolUse.Name
				out <- EventToolStart{ID: st.toolID, Name: st.toolName}
			}
		case "contentBlockDelta":
			var d struct {
				ContentBlockIndex int `json:"contentBlockIndex"`
				Delta             struct {
					Text    string `json:"text"`
					ToolUse *struct {
						Input string `json:"input"`
					} `json:"toolUse"`
				} `json:"delta"`
			}
			if err := unmarshalBedrockEventPayload(evt.payload, "contentBlockDelta", &d); err != nil {
				continue
			}
			st := contentBlocks[d.ContentBlockIndex]
			if st == nil {
				st = &bedrockBlockState{}
				contentBlocks[d.ContentBlockIndex] = st
			}
			if d.Delta.Text != "" {
				st.text.WriteString(d.Delta.Text)
				out <- EventTextDelta{Delta: d.Delta.Text}
			}
			if d.Delta.ToolUse != nil && d.Delta.ToolUse.Input != "" {
				st.toolArgs.WriteString(d.Delta.ToolUse.Input)
				out <- EventToolArgs{ID: st.toolID, Delta: d.Delta.ToolUse.Input}
			}
		case "contentBlockStop":
			var d struct {
				ContentBlockIndex int `json:"contentBlockIndex"`
			}
			if err := unmarshalBedrockEventPayload(evt.payload, "contentBlockStop", &d); err != nil {
				continue
			}
			st := contentBlocks[d.ContentBlockIndex]
			if st == nil {
				continue
			}
			if st.isToolUse {
				args := st.toolArgs.String()
				if args == "" {
					args = "{}"
				}
				finalMsg.Content = append(finalMsg.Content, ToolCallBlock{
					ID: st.toolID, Name: st.toolName, Arguments: json.RawMessage(args),
				})
				out <- EventToolEnd{ID: st.toolID}
			} else if st.text.Len() > 0 {
				finalMsg.Content = append(finalMsg.Content, TextBlock{Text: st.text.String()})
			}
		case "messageStop":
			var d struct {
				StopReason string `json:"stopReason"`
			}
			_ = unmarshalBedrockEventPayload(evt.payload, "messageStop", &d)
			switch d.StopReason {
			case "tool_use":
				stop = StopToolUse
			case "end_turn":
				stop = StopEnd
			case "max_tokens":
				stop = StopLength
			default:
				stop = StopEnd
			}
		case "metadata":
			var d struct {
				Usage struct {
					InputTokens           int `json:"inputTokens"`
					OutputTokens          int `json:"outputTokens"`
					CacheReadInputTokens  int `json:"cacheReadInputTokens"`
					CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
				} `json:"usage"`
			}
			if err := unmarshalBedrockEventPayload(evt.payload, "metadata", &d); err == nil {
				usage.InputTokens = d.Usage.InputTokens
				usage.OutputTokens = d.Usage.OutputTokens
				usage.CacheReadTokens = d.Usage.CacheReadInputTokens
				usage.CacheWriteTokens = d.Usage.CacheWriteInputTokens
				if m, err := FindModel("amazon-bedrock", req.Model); err == nil {
					usage.CostUSD = ComputeCost(m, usage)
				}
				out <- EventUsage{Usage: usage}
			}
		}
	}
	out <- EventDone{Stop: stop, Message: finalMsg}
}

type bedrockBlockState struct {
	isToolUse bool
	toolID    string
	toolName  string
	toolArgs  strings.Builder
	text      strings.Builder
}

func bedrockEventTypeFromPayload(payload []byte) string {
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(payload, &outer); err != nil {
		return ""
	}
	for _, name := range []string{"messageStart", "contentBlockStart", "contentBlockDelta", "contentBlockStop", "messageStop", "metadata"} {
		if _, ok := outer[name]; ok {
			return name
		}
	}
	return ""
}

func unmarshalBedrockEventPayload(payload []byte, eventType string, dst any) error {
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(payload, &outer); err == nil {
		if wrapped, ok := outer[eventType]; ok {
			return json.Unmarshal(wrapped, dst)
		}
	}
	if err := json.Unmarshal(payload, dst); err != nil {
		return fmt.Errorf("bedrock: parse %s payload: %w", eventType, err)
	}
	return nil
}

// ---- event-stream binary framing parser ----

type eventStreamMessage struct {
	headers map[string]string
	payload []byte
}

func (m *eventStreamMessage) headerString(name string) string { return m.headers[name] }

// readEventStreamMessage reads one event-stream message from r. Returns
// io.EOF when the stream is finished cleanly.
func readEventStreamMessage(r io.Reader) (*eventStreamMessage, error) {
	var prelude [8]byte
	if _, err := io.ReadFull(r, prelude[:]); err != nil {
		return nil, err
	}
	totalLen := binary.BigEndian.Uint32(prelude[0:4])
	headersLen := binary.BigEndian.Uint32(prelude[4:8])
	if totalLen < 16 || headersLen > totalLen-16 {
		return nil, fmt.Errorf("bedrock: malformed event-stream prelude (total=%d headers=%d)", totalLen, headersLen)
	}
	// Skip prelude CRC (4 bytes); we don't validate.
	var preludeCRC [4]byte
	if _, err := io.ReadFull(r, preludeCRC[:]); err != nil {
		return nil, err
	}
	headersBuf := make([]byte, headersLen)
	if _, err := io.ReadFull(r, headersBuf); err != nil {
		return nil, err
	}
	payloadLen := int(totalLen) - 16 - int(headersLen)
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	var trailer [4]byte
	if _, err := io.ReadFull(r, trailer[:]); err != nil {
		return nil, err
	}
	// Parse headers.
	hdrs := map[string]string{}
	i := 0
	for i < len(headersBuf) {
		if i+1 > len(headersBuf) {
			break
		}
		nameLen := int(headersBuf[i])
		i++
		if i+nameLen > len(headersBuf) {
			return nil, fmt.Errorf("bedrock: header name overflow")
		}
		name := string(headersBuf[i : i+nameLen])
		i += nameLen
		if i+1 > len(headersBuf) {
			return nil, fmt.Errorf("bedrock: header value-type missing")
		}
		valueType := headersBuf[i]
		i++
		// Type 7 = string with 2-byte length. Type 6 = byte-array similar.
		// Other types (bool/int/long/timestamp/uuid) we treat as opaque
		// strings since bedrock's event-type headers are always type 7.
		switch valueType {
		case 7, 6:
			if i+2 > len(headersBuf) {
				return nil, fmt.Errorf("bedrock: header value length missing")
			}
			vLen := int(binary.BigEndian.Uint16(headersBuf[i : i+2]))
			i += 2
			if i+vLen > len(headersBuf) {
				return nil, fmt.Errorf("bedrock: header value overflow")
			}
			hdrs[name] = string(headersBuf[i : i+vLen])
			i += vLen
		case 0: // bool true
			hdrs[name] = "true"
		case 1: // bool false
			hdrs[name] = "false"
		case 2: // byte
			if i+1 > len(headersBuf) {
				return nil, fmt.Errorf("bedrock: header byte missing")
			}
			i++
		case 3: // int16
			if i+2 > len(headersBuf) {
				return nil, fmt.Errorf("bedrock: header int16 missing")
			}
			i += 2
		case 4: // int32
			if i+4 > len(headersBuf) {
				return nil, fmt.Errorf("bedrock: header int32 missing")
			}
			i += 4
		case 5: // int64
			if i+8 > len(headersBuf) {
				return nil, fmt.Errorf("bedrock: header int64 missing")
			}
			i += 8
		case 8: // timestamp (int64)
			if i+8 > len(headersBuf) {
				return nil, fmt.Errorf("bedrock: header timestamp missing")
			}
			i += 8
		case 9: // uuid (16 bytes)
			if i+16 > len(headersBuf) {
				return nil, fmt.Errorf("bedrock: header uuid missing")
			}
			i += 16
		default:
			return nil, fmt.Errorf("bedrock: unknown header value-type %d", valueType)
		}
	}
	return &eventStreamMessage{headers: hdrs, payload: payload}, nil
}

// ---- SigV4 signing ----
//
// AWS Signature Version 4. Implemented from scratch so we don't pull in
// aws-sdk-go-v2 (large transitive dep tree just for one signer). The
// algorithm is documented at:
//   https://docs.aws.amazon.com/general/latest/gr/sigv4-signed-request-examples.html
//
// Steps:
//
//   1. Build the canonical request: method + path + canonical query +
//      canonical headers + signed headers + payload-hash.
//   2. Build the string-to-sign: algorithm + amz-date + credential scope +
//      sha256(canonical request).
//   3. Derive the signing key by chaining HMAC-SHA256:
//      kSecret -> kDate -> kRegion -> kService -> kSigning
//   4. signature = HMAC-SHA256(kSigning, stringToSign)
//   5. Authorization header: AWS4-HMAC-SHA256 Credential=..., SignedHeaders=..., Signature=...
//
// Payload hash is sha256 of the JSON body. For Bedrock's streaming
// endpoint this works because the request body is fully buffered before
// the call — we don't need the streaming-payload variant.

func sigHMAC(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

func sigSHA256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func signSigV4(req *http.Request, payload []byte, service, region string, creds *bedrockSigV4Creds, now time.Time) error {
	amzDate := now.Format("20060102T150405Z")
	shortDate := now.Format("20060102")
	credentialScope := shortDate + "/" + region + "/" + service + "/aws4_request"

	// Required headers.
	req.Header.Set("host", req.URL.Host)
	req.Header.Set("x-amz-date", amzDate)
	if creds.sessionToken != "" {
		req.Header.Set("x-amz-security-token", creds.sessionToken)
	}
	payloadHash := sigSHA256Hex(payload)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	// Canonical headers: lowercase name, trimmed value, sorted by name,
	// each on its own line ending with \n.
	var names []string
	values := map[string]string{}
	for k, vs := range req.Header {
		lower := strings.ToLower(k)
		// Skip authorization (we're computing it) and any
		// connection-specific headers.
		if lower == "authorization" {
			continue
		}
		names = append(names, lower)
		values[lower] = strings.Join(vs, ",")
	}
	// host is special — it's not in req.Header by default; set above.
	sort.Strings(names)
	var canonHdrs strings.Builder
	for _, n := range names {
		canonHdrs.WriteString(n)
		canonHdrs.WriteByte(':')
		canonHdrs.WriteString(strings.TrimSpace(values[n]))
		canonHdrs.WriteByte('\n')
	}
	signedHeaders := strings.Join(names, ";")

	// Canonical query string: keys sorted by name, each key and value
	// URI-escaped (RFC 3986).
	q := req.URL.Query()
	var qKeys []string
	for k := range q {
		qKeys = append(qKeys, k)
	}
	sort.Strings(qKeys)
	var canonQuery strings.Builder
	for i, k := range qKeys {
		if i > 0 {
			canonQuery.WriteByte('&')
		}
		canonQuery.WriteString(url.QueryEscape(k))
		canonQuery.WriteByte('=')
		canonQuery.WriteString(url.QueryEscape(q.Get(k)))
	}

	// Canonical request.
	canonReq := req.Method + "\n" +
		canonicalSigV4Path(req.URL.Path) + "\n" +
		canonQuery.String() + "\n" +
		canonHdrs.String() + "\n" +
		signedHeaders + "\n" +
		payloadHash

	// String to sign.
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + credentialScope + "\n" + sigSHA256Hex([]byte(canonReq))

	// Signing key.
	kDate := sigHMAC([]byte("AWS4"+creds.secretAccessKey), []byte(shortDate))
	kRegion := sigHMAC(kDate, []byte(region))
	kService := sigHMAC(kRegion, []byte(service))
	kSigning := sigHMAC(kService, []byte("aws4_request"))

	// Signature.
	signature := hex.EncodeToString(sigHMAC(kSigning, []byte(stringToSign)))

	// Authorization header.
	auth := "AWS4-HMAC-SHA256 " +
		"Credential=" + creds.accessKeyID + "/" + credentialScope + ", " +
		"SignedHeaders=" + signedHeaders + ", " +
		"Signature=" + signature
	req.Header.Set("Authorization", auth)
	return nil
}

// canonicalSigV4Path returns the path with each segment URI-encoded
// according to SigV4 rules (everything except unreserved chars
// A-Za-z0-9-_.~ and / between segments). Bedrock model IDs contain
// dots, colons, and slashes; the colons must be percent-encoded.
func canonicalSigV4Path(p string) string {
	if p == "" {
		return "/"
	}
	segments := strings.Split(p, "/")
	for i, s := range segments {
		segments[i] = sigV4EncodeSegment(s)
	}
	return strings.Join(segments, "/")
}

func sigV4EncodeSegment(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		// Unreserved per RFC 3986.
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}
