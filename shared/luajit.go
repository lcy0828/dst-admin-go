package shared

// LuaJIT packages belong to the Runtime that downloads or installs them.
type LuaJITRelease struct {
	Channel   string `json:"channel,omitempty"`
	SourceURL string `json:"sourceUrl,omitempty"`
	ID        string `json:"id"`
	Version   string `json:"version"`
	Revision  string `json:"revision,omitempty"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}

// Install normally carries only ReleaseID. SourceURL downloads directly on the
// Runtime. Release + DownloadPath + DownloadToken explicitly opt into relay.
type RuntimeLuaJITRequest struct {
	RefreshCatalog bool           `json:"refresh_catalog,omitempty"`
	ReleaseID      string         `json:"release_id,omitempty"`
	SourceURL      string         `json:"source_url,omitempty"`
	SHA256         string         `json:"sha256,omitempty"`
	Release        *LuaJITRelease `json:"release,omitempty"`
	DownloadPath   string         `json:"download_path,omitempty"`
	DownloadToken  string         `json:"download_token,omitempty"`
}
