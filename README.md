# show

Stream any command’s stdout to a browser with accurate terminal rendering.

## Install
- Go: `go install github.com/elimelt/show/cmd/show@latest`
- Homebrew (homebrew-core): planned
- macOS: `curl -fsSL https://raw.githubusercontent.com/elimelt/show/main/install/macos.sh | sudo bash`
- Linux: `curl -fsSL https://raw.githubusercontent.com/elimelt/show/main/install/linux.sh | sudo bash`

## Usage
- Pipe and mirror to terminal: `ls --color=always -la | show -p 8000` → open `http://localhost:8000`
- Interactive (PTY) mode:
  - Split args: `show -pty -- sudo cat`
  - Single command string: `show -pty -- "sudo cat"`

## Flags
- `-p` port (default 8000)
- `-host` bind address (default 127.0.0.1)
- `-title` page title
- `-history` replay bytes (default 16MB, 0 = unlimited)
- `-version` print version and exit
- `-pty` run a command under a PTY and mirror to terminal (accepts either split args or a single shell command string)
- `-input` allow browser keyboard input (PTY mode)

Notes: Piped commands may disable color; use `--color=always`. For interactive TUIs, use `-pty`.

## Examples
- Interactive shell in the browser:
  - `show -pty -input -- script -q -c "bash" | tee /dev/stdout`
- TUI with keyboard input in browser:
  - `show -pty -input -- btop`
- Remote over SSH port forwarding:
  - `ssh -L 8000:<hostname>:8000 <user>@<hostname>`
  - On remote: `ps aux | show -p 8000 -host 0.0.0.0`

## Author
Elijah Melton
