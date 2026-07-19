package model

// GlobalDisabledModel 是跨全部渠道生效的模型禁用规则。
type GlobalDisabledModel struct {
	Model     string `json:"model"`
	Note      string `json:"note,omitempty"`
	CreatedAt int64  `json:"created_at"`
}
