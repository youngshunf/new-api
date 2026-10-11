package dto

type VoiceCreateRequest struct {
	Kind        string `json:"kind"`
	Model       string `json:"model"`
	Language    string `json:"language"`
	Description string `json:"description,omitempty"`
	PreviewText string `json:"preview_text,omitempty"`
	Reference   []byte `json:"-"`
	Digest      string `json:"-"`
	Key         string `json:"-"`
}

type VoiceResource struct {
	VoiceRef         string  `json:"voice_ref"`
	Kind             string  `json:"kind"`
	Model            string  `json:"model"`
	State            string  `json:"state"`
	FailureReason    *string `json:"failure_reason"`
	PreviewAvailable bool    `json:"preview_available"`
	RetentionPolicy  string  `json:"retention_policy"`
	DeleteSupported  bool    `json:"delete_supported"`
	DeletePending    bool    `json:"delete_pending"`
}
