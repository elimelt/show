# show

Stream any command’s stdout to a browser with accurate terminal rendering.

## Install
- Go: `go install github.com/elimelt/show/cmd/show@latest`
- Homebrew (homebrew-core): planned
- macOS: `curl -fsSL https://raw.githubusercontent.com/elimelt/show/main/install/macos.sh | sudo bash`
- Linux: `curl -fsSL https://raw.githubusercontent.com/elimelt/show/main/install/linux.sh | sudo bash`

## Usage
- Pipe and mirror to terminal: `ls --color=always -la | show -p 8000` → open `http://localhost:8000`
- Interactive (PTY) mode: `show -pty -- btop`

## Flags
- `-p` port (default 8000)
- `-host` bind address (default 127.0.0.1)
- `-title` page title
- `-history` replay bytes (default 16MB, 0 = unlimited)
- `-version` print version and exit
- `-pty` run a command under a PTY and mirror to terminal

Notes: Piped commands may disable color; use `--color=always`. For interactive TUIs, use `-pty`.

## Author
Elijah Melton
