// config.go

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"

	infisical "github.com/infisical/go-sdk"
	"github.com/infisical/go-sdk/packages/models"
	"github.com/tailscale/hujson"
)

// config struct for loading a configuration file
type config struct {
	GoogleGenerativeModel string `json:"google_generative_model,omitempty"`

	MonitorIntervalSeconds  int    `json:"monitor_interval_seconds"`
	TelegramIntervalSeconds int    `json:"telegram_interval_seconds"`
	MaxNumTries             int    `json:"max_num_tries"`
	DBFilepath              string `json:"db_filepath"`

	// other optional configurations
	AllowedTelegramUsers []string `json:"allowed_telegram_users"`
	DefaultHour          int      `json:"default_hour,omitempty"`
	Verbose              bool     `json:"verbose,omitempty"`

	// token and api key
	TelegramBotToken *string `json:"telegram_bot_token,omitempty"`
	GoogleAIAPIKey   *string `json:"google_ai_api_key,omitempty"`

	// or Infisical settings
	Infisical *struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`

		ProjectID   string `json:"project_id"`
		Environment string `json:"environment"`
		SecretType  string `json:"secret_type"`

		TelegramBotTokenKeyPath string `json:"telegram_bot_token_key_path"`
		GoogleAIAPIKeyKeyPath   string `json:"google_ai_api_key_key_path"`
	} `json:"infisical,omitempty"`
}

// load config at given path
func loadConfig(fpath string) (conf config, err error) {
	var bytes []byte
	if bytes, err = os.ReadFile(fpath); err == nil {
		if bytes, err = standardizeJSON(bytes); err == nil {
			if err = json.Unmarshal(bytes, &conf); err == nil {
				if (conf.TelegramBotToken == nil || conf.GoogleAIAPIKey == nil) &&
					conf.Infisical != nil {
					// read token and api key from infisical
					client := infisical.NewInfisicalClient(context.TODO(), infisical.Config{
						SiteUrl: "https://app.infisical.com",
					})

					_, err = client.Auth().UniversalAuthLogin(conf.Infisical.ClientID, conf.Infisical.ClientSecret)
					if err != nil {
						return config{}, fmt.Errorf("failed to authenticate with Infisical: %s", err)
					}

					var keyPath string
					var secret models.Secret

					// telegram bot token
					keyPath = conf.Infisical.TelegramBotTokenKeyPath
					secret, err = client.Secrets().Retrieve(infisical.RetrieveSecretOptions{
						ProjectID:   conf.Infisical.ProjectID,
						Type:        conf.Infisical.SecretType,
						Environment: conf.Infisical.Environment,
						SecretPath:  path.Dir(keyPath),
						SecretKey:   path.Base(keyPath),
					})
					if err == nil {
						val := secret.SecretValue
						conf.TelegramBotToken = &val
					} else {
						return config{}, fmt.Errorf("failed to retrieve `telegram_bot_token` from Infisical: %s", err)
					}

					// google ai api key
					keyPath = conf.Infisical.GoogleAIAPIKeyKeyPath
					secret, err = client.Secrets().Retrieve(infisical.RetrieveSecretOptions{
						ProjectID:   conf.Infisical.ProjectID,
						Type:        conf.Infisical.SecretType,
						Environment: conf.Infisical.Environment,
						SecretPath:  path.Dir(keyPath),
						SecretKey:   path.Base(keyPath),
					})
					if err == nil {
						val := secret.SecretValue
						conf.GoogleAIAPIKey = &val
					} else {
						return config{}, fmt.Errorf("failed to retrieve `google_ai_api_key` from Infisical: %s", err)
					}
				}

				// set default/fallback values
				if conf.MonitorIntervalSeconds <= 0 {
					conf.MonitorIntervalSeconds = defaultMonitorIntervalSeconds
				}
				if conf.TelegramIntervalSeconds <= 0 {
					conf.TelegramIntervalSeconds = defaultTelegramIntervalSeconds
				}
				if conf.MaxNumTries <= 0 {
					conf.MaxNumTries = defaultMaxNumTries
				}
				if conf.GoogleGenerativeModel == "" {
					conf.GoogleGenerativeModel = defaultGenerativeModel
				}
				if conf.DefaultHour < 0 || conf.DefaultHour >= 24 {
					conf.DefaultHour = 0
				}
			}
		}
	}

	return conf, err
}

// standardize given JSON (JWCC) bytes
func standardizeJSON(b []byte) ([]byte, error) {
	ast, err := hujson.Parse(b)
	if err != nil {
		return b, err
	}
	ast.Standardize()

	return ast.Pack(), nil
}
