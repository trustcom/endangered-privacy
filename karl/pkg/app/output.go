package app

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"karl/pkg/config"
)

type output struct {
	Result any
	Prefix string
	Suffix string
	Error  error
}

type jsonWriter struct {
	config        *config.AppConfig
	fileFormatStr string
}

func newJSONWriter(config *config.AppConfig) (*jsonWriter, error) {
	if err := os.MkdirAll(config.OutDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}

	var (
		now           = time.Now().UTC()
		fileFormatStr = "%s" + now.Format("20060102_150405") + "%s.json"
	)

	return &jsonWriter{
		config:        config,
		fileFormatStr: fileFormatStr,
	}, nil
}

func (jw *jsonWriter) write(output output) error {
	var (
		filename = fmt.Sprintf(jw.fileFormatStr, output.Prefix, output.Suffix)
		path     = filepath.Join(jw.config.OutDir, filename)
	)
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	if !jw.config.NoIndent {
		encoder.SetIndent("", "  ")
	}
	if err := encoder.Encode(output.Result); err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}

	log.Printf("Saved %s\n", path)
	return nil
}
