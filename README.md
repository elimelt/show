# show

Stream any command’s stdout to a browser with accurate terminal rendering.

## Install
- Go: `go install github.com/elimelt/show/cmd/show@latest`
- Homebrew (homebrew-core): planned
- macOS: `curl -fsSL https://raw.githubusercontent.com/elimelt/show/main/install/macos.sh | bash`
- Linux: `curl -fsSL https://raw.githubusercontent.com/elimelt/show/main/install/linux.sh | bash`

## Usage
- `ls --color=always -la | show -p 8000` → open `http://localhost:8000`
- `top -b | show`

## Flags
- `-p` port (default 8000)
- `-host` bind address (default 127.0.0.1)
- `-title` page title
- `-history` replay bytes (default 16MB, 0 = unlimited)
- `-version` print version and exit

Notes: Some commands disable color when piped; use `--color=always`. Interactive TUIs may require batch/non-interactive modes for best results.

## Author
Elijah Melton
