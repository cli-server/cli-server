package ccbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

type VikingClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func NewVikingClient(baseURL, apiKey string) *VikingClient {
	return &VikingClient{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
}

// vikingResponse is the generic OpenViking API response wrapper
type vikingResponse struct {
	Status string          `json:"status"`
	Result json.RawMessage `json:"result"`
}

type vikingFileEntry struct {
	Name    string `json:"name"`
	IsDir   bool   `json:"isDir"`
	URI     string `json:"uri"`
	RelPath string `json:"rel_path"`
}

func (v *VikingClient) doRequest(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, v.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if v.apiKey != "" {
		req.Header.Set("X-API-Key", v.apiKey)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return v.httpClient.Do(req)
}

// DownloadTree recursively downloads files from a Viking URI to a local directory.
func (v *VikingClient) DownloadTree(ctx context.Context, vikingURI, localDir string) error {
	// 1. List files recursively
	listURL := fmt.Sprintf("/api/v1/fs/ls?uri=%s&recursive=true", url.QueryEscape(vikingURI))
	resp, err := v.doRequest(ctx, "GET", listURL, nil, "")
	if err != nil {
		return fmt.Errorf("list files: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		// Directory might not exist yet — that's OK for first-time workspaces
		return nil
	}

	var vresp vikingResponse
	if err := json.NewDecoder(resp.Body).Decode(&vresp); err != nil {
		return fmt.Errorf("decode list response: %w", err)
	}

	var entries []vikingFileEntry
	if err := json.Unmarshal(vresp.Result, &entries); err != nil {
		return fmt.Errorf("unmarshal entries: %w", err)
	}

	// 2. Create directories and download files
	for _, entry := range entries {
		localPath := filepath.Join(localDir, entry.RelPath)
		if entry.IsDir {
			os.MkdirAll(localPath, 0755)
			continue
		}

		// Download file content
		readURL := fmt.Sprintf("/api/v1/content/read?uri=%s", url.QueryEscape(entry.URI))
		fresp, err := v.doRequest(ctx, "GET", readURL, nil, "")
		if err != nil {
			continue // skip failed files
		}

		var fvresp vikingResponse
		json.NewDecoder(fresp.Body).Decode(&fvresp)
		fresp.Body.Close()

		// Result is the file content as a JSON string
		var content string
		json.Unmarshal(fvresp.Result, &content)

		os.MkdirAll(filepath.Dir(localPath), 0755)
		os.WriteFile(localPath, []byte(content), 0644)
	}

	return nil
}

// UploadFile writes content to an existing file in OpenViking.
func (v *VikingClient) UploadFile(ctx context.Context, vikingURI, content string) error {
	body, _ := json.Marshal(map[string]interface{}{
		"uri":     vikingURI,
		"content": content,
		"mode":    "replace",
		"wait":    false,
	})
	resp, err := v.doRequest(ctx, "POST", "/api/v1/content/write", bytes.NewReader(body), "application/json")
	if err != nil {
		return fmt.Errorf("upload file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload failed (status %d): %s", resp.StatusCode, respBody)
	}
	return nil
}

// CreateFile creates a new file in OpenViking using temp_upload + add_resource.
func (v *VikingClient) CreateFile(ctx context.Context, vikingURI string, content []byte) error {
	// Step 1: temp_upload
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", "upload.dat")
	if err != nil {
		return err
	}
	part.Write(content)
	writer.Close()

	resp, err := v.doRequest(ctx, "POST", "/api/v1/resources/temp_upload", &buf, writer.FormDataContentType())
	if err != nil {
		return fmt.Errorf("temp upload: %w", err)
	}
	defer resp.Body.Close()

	var uploadResp vikingResponse
	json.NewDecoder(resp.Body).Decode(&uploadResp)

	var uploadResult struct {
		TempFileID string `json:"temp_file_id"`
	}
	json.Unmarshal(uploadResp.Result, &uploadResult)

	if uploadResult.TempFileID == "" {
		return fmt.Errorf("temp upload returned no file ID")
	}

	// Step 2: add_resource
	addBody, _ := json.Marshal(map[string]interface{}{
		"temp_file_id": uploadResult.TempFileID,
		"to":           vikingURI,
		"wait":         false,
	})
	resp2, err := v.doRequest(ctx, "POST", "/api/v1/resources", bytes.NewReader(addBody), "application/json")
	if err != nil {
		return fmt.Errorf("add resource: %w", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp2.Body)
		return fmt.Errorf("add resource failed (status %d): %s", resp2.StatusCode, respBody)
	}
	return nil
}
