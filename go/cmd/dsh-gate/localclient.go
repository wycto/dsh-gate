package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 本机控制 API 的客户端。服务端在 internal/portal/localapi.go：
// 只接受回环来源 + <状态目录>/portal.token 里的 Bearer 令牌。

// localAPIPath 是本机控制 API 的路径（与服务端常量保持一致）。
const localAPIPath = "/_portal/api/local"

// localInstanceInfo 是本机 API 返回的一个实例。
type localInstanceInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	DshHome    string `json:"dshHome"`
	ConfigDir  string `json:"configDir"`
	Workspace  string `json:"workspace"`
	InstallDir string `json:"installDir"`
	Port       int    `json:"port"`
	Enabled    bool   `json:"enabled"`
	Running    bool   `json:"running"`
	Ready      bool   `json:"ready"`
	PID        int    `json:"pid"`
	Error      string `json:"error"`
	URL        string `json:"url"`
}

// localResponse 是本机 API 的统一响应体。
type localResponse struct {
	OK        bool                `json:"ok"`
	Error     string              `json:"error"`
	Instances []localInstanceInfo `json:"instances"`
}

// localRequest 是本机 API 的请求体。
type localRequest struct {
	Action       string `json:"action"`
	Instance     string `json:"instance,omitempty"`
	PurgeHome    bool   `json:"purgeHome,omitempty"`
	PurgeInstall bool   `json:"purgeInstall,omitempty"`
}

// localEndpoint 找到管理端的回环端口与令牌。
//
// 管理端没在运行（或令牌文件缺失，例如升级前启动的旧进程）时返回可读错误：
// 调用方据此决定「提示先启动管理端」还是「退回直接改库」。
func localEndpoint(stateDir string) (int, string, error) {
	record, ok := portalRecord(resolveRunDir(""))
	if !ok {
		return 0, "", fmt.Errorf("管理端没有在运行：先 dsh-gate start")
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, localTokenName))
	if err != nil {
		return 0, "", fmt.Errorf("读不到本机控制令牌（%s）：管理端可能还在用旧版本，重启一次 dsh-gate 即可", filepath.Join(stateDir, localTokenName))
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return 0, "", fmt.Errorf("本机控制令牌是空的：重启一次管理端")
	}
	return record.Port, token, nil
}

// localPost 发一个本机控制请求并解析响应。
func localPost(stateDir string, req localRequest) (localResponse, error) {
	var out localResponse
	port, token, err := localEndpoint(stateDir)
	if err != nil {
		return out, err
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + localAPIPath
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(httpReq)
	if err != nil {
		return out, fmt.Errorf("连不上管理端本机接口（%s）：%w", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("管理端返回了非 JSON 响应（HTTP %d）：%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if !out.OK {
		if out.Error == "" {
			out.Error = fmt.Sprintf("管理端返回 HTTP %d", resp.StatusCode)
		}
		return out, fmt.Errorf("%s", out.Error)
	}
	return out, nil
}

// localListInstances 让管理端返回实例定义 + 实时运行状态。
func localListInstances(stateDir string) ([]localInstanceInfo, error) {
	out, err := localPost(stateDir, localRequest{Action: "list"})
	if err != nil {
		return nil, err
	}
	return out.Instances, nil
}
