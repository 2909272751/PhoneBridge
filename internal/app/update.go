package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const latestReleaseURL = "https://api.github.com/repos/2909272751/PhoneBridge/releases/latest"

type releaseSnapshot struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

func (server *Server) handleUpdateCheck(writer http.ResponseWriter, request *http.Request) {
	client := &http.Client{Timeout: 5 * time.Second}
	upstream, err := http.NewRequestWithContext(request.Context(), http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "无法创建更新检查请求")
		return
	}
	upstream.Header.Set("Accept", "application/vnd.github+json")
	upstream.Header.Set("User-Agent", "PhoneBridge/"+version)
	response, err := client.Do(upstream)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "暂时无法连接更新服务；请稍后重试")
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		writeError(writer, http.StatusBadGateway, fmt.Sprintf("更新服务返回 %s", response.Status))
		return
	}
	var release releaseSnapshot
	if err = json.NewDecoder(http.MaxBytesReader(writer, response.Body, 1<<20)).Decode(&release); err != nil || release.TagName == "" || release.HTMLURL == "" {
		writeError(writer, http.StatusBadGateway, "更新信息格式无效")
		return
	}
	latest := strings.TrimPrefix(strings.TrimSpace(release.TagName), "v")
	writeJSON(writer, http.StatusOK, map[string]any{
		"currentVersion":  version,
		"latestVersion":   latest,
		"updateAvailable": versionNewer(latest, version),
		"releaseURL":      release.HTMLURL,
	})
}

func versionNewer(candidate, current string) bool {
	parts := func(value string) [3]int {
		var result [3]int
		for index, item := range strings.Split(value, ".") {
			if index >= len(result) {
				break
			}
			result[index], _ = strconv.Atoi(item)
		}
		return result
	}
	a, b := parts(candidate), parts(current)
	for index := range a {
		if a[index] != b[index] {
			return a[index] > b[index]
		}
	}
	return false
}
