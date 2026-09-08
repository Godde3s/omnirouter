// models.go — /v1/models for the DeepSeek bridge (static catalog).

package dsbridge

import (
	"net/http"
	"time"
)

func modelsHandler(w http.ResponseWriter, r *http.Request) {
	created := time.Now().Unix()
	data := make([]map[string]interface{}, 0, len(dsModels))
	for _, m := range dsModels {
		data = append(data, map[string]interface{}{
			"id":          m.ID,
			"object":      "model",
			"created":     created,
			"owned_by":    "deepseek",
			"description": m.Description,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"object": "list", "data": data})
}

func modelsHandler2(w http.ResponseWriter, r *http.Request) {
	modelsHandler(w, r)
}

func modelSupportsVision(modelID string) bool {
	return false
}
