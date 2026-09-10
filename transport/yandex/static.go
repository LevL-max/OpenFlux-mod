package yandex

import (
	"encoding/json"
	"fmt"
	"os"
)

type staticYandexDocConfig struct {
	Token       string                 `json:"token"`
	DocID       string                 `json:"doc_id"`
	Origin      string                 `json:"origin"`
	Host        string                 `json:"host"`
	WsURL       string                 `json:"ws_url"`
	Permissions map[string]interface{} `json:"permissions"`
	OpenCmd     map[string]interface{} `json:"open_cmd"`
}

func loadStaticDocInfo(path, userID string) (YandexDocsInfo, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return YandexDocsInfo{}, fmt.Errorf("static config read failed: %w", err)
	}

	var c staticYandexDocConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return YandexDocsInfo{}, fmt.Errorf("static config parse failed: %w", err)
	}

	if c.Token == "" || c.DocID == "" || c.Origin == "" ||
		c.Host == "" || c.WsURL == "" {
		return YandexDocsInfo{}, fmt.Errorf("static config missing required fields")
	}

	if c.OpenCmd == nil {
		c.OpenCmd = make(map[string]interface{})
	}
	c.OpenCmd["userid"] = userID

	return YandexDocsInfo{
		Token:       c.Token,
		DocID:       c.DocID,
		UserID:      userID,
		Origin:      c.Origin,
		Host:        c.Host,
		WsURL:       c.WsURL,
		Permissions: c.Permissions,
		OpenCmd:     c.OpenCmd,
	}, nil
}
