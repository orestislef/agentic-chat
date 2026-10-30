package server

import (
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
)

// The hub ships the client binary for every platform, so a self hosted setup
// depends on nothing but itself.
var downloadRE = regexp.MustCompile(`^agentic-chat-(linux|darwin|windows)-(amd64|arm64)(\.exe)?$`)

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	if !downloadRE.MatchString(file) {
		writeError(w, http.StatusNotFound, "no such download")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	http.ServeFile(w, r, filepath.Join(s.cfg.DownloadsDir, file))
}

const installShell = `#!/bin/sh
# Installs the agentic-chat client into ~/.local/bin
set -eu

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "unsupported system: $(uname -s), use install.ps1 on Windows" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

dir="$HOME/.local/bin"
mkdir -p "$dir"
curl -fsSL "%[1]s/downloads/agentic-chat-$os-$arch" -o "$dir/agentic-chat.tmp"
chmod +x "$dir/agentic-chat.tmp"
mv "$dir/agentic-chat.tmp" "$dir/agentic-chat"
echo "installed $dir/agentic-chat"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "add $dir to your PATH" ;;
esac
`

const installPowerShell = `# Installs the agentic-chat client into %%LOCALAPPDATA%%\agentic-chat
$ErrorActionPreference = 'Stop'
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$dir = Join-Path $env:LOCALAPPDATA 'agentic-chat'
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$target = Join-Path $dir 'agentic-chat.exe'
Invoke-WebRequest -UseBasicParsing "%[1]s/downloads/agentic-chat-windows-$arch.exe" -OutFile $target
$path = [Environment]::GetEnvironmentVariable('Path', 'User')
if (($path -split ';') -notcontains $dir) {
  [Environment]::SetEnvironmentVariable('Path', "$path;$dir", 'User')
  Write-Host "added $dir to your PATH, open a new terminal to use it"
}
Write-Host "installed $target"
`

func (s *Server) handleInstallShell(w http.ResponseWriter, r *http.Request) {
	s.writeInstaller(w, r, installShell)
}

func (s *Server) handleInstallPowerShell(w http.ResponseWriter, r *http.Request) {
	s.writeInstaller(w, r, installPowerShell)
}

func (s *Server) writeInstaller(w http.ResponseWriter, r *http.Request, script string) {
	base, ok := s.baseURL(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad host header")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, script, base)
}
