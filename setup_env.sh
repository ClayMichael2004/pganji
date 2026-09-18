#!/usr/bin/env bash
set -euo pipefail

echo "====================================================="
echo "   PGANJI AUTOMATED ENVIRONMENT & DB BOOTSTRAPPER   "
echo "====================================================="

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PG_DATA="$HOME/my_postgres"
PG_SOCKETS="$PG_DATA/sockets"
PG_PORT="5433"
MINICONDA_DIR="$HOME/miniconda3"
ZSHRC="$HOME/.zshrc"

# -----------------------------------------------------------------------------
# 1. Miniconda Installation & Verification
# -----------------------------------------------------------------------------
if [ ! -d "$MINICONDA_DIR" ] && ! command -v conda &>/dev/null; then
    echo "[+] Miniconda not detected. Installing Miniconda to $MINICONDA_DIR..."
    MINICONDA_INSTALLER="/tmp/miniconda_installer_$$.sh"
    curl -fsSL https://repo.anaconda.com/miniconda/Miniconda3-latest-Linux-x86_64.sh -o "$MINICONDA_INSTALLER"
    bash "$MINICONDA_INSTALLER" -b -p "$MINICONDA_DIR"
    rm -f "$MINICONDA_INSTALLER"
fi

# Load Conda into current execution subshell
if [ -f "$MINICONDA_DIR/etc/profile.d/conda.sh" ]; then
    source "$MINICONDA_DIR/etc/profile.d/conda.sh"
elif command -v conda &>/dev/null; then
    eval "$(conda shell.bash hook)"
fi

if command -v conda &>/dev/null; then
    conda activate base || true
fi

# -----------------------------------------------------------------------------
# 2. PostgreSQL Installation via Conda (No Sudo)
# -----------------------------------------------------------------------------
if ! command -v initdb &>/dev/null; then
    echo "[+] Installing PostgreSQL into user conda environment..."
    conda install --override-channels -c conda-forge postgresql -y
else
    echo "[✓] PostgreSQL binaries already available at $(which initdb)."
fi

# -----------------------------------------------------------------------------
# 3. User Go Environment & Paths Setup (No Sudo)
# -----------------------------------------------------------------------------
echo "[+] Configuring user-space Go environment..."
mkdir -p "$HOME/go/pkg/mod" "$HOME/.cache/go-build"

export GOPATH="$HOME/go"
export GOMODCACHE="$HOME/go/pkg/mod"
export GOCACHE="$HOME/.cache/go-build"
export GOTOOLCHAIN="local"

# Append exports and convenience aliases to ~/.zshrc if missing
if [ -f "$ZSHRC" ]; then
    grep -q "GOTOOLCHAIN=local" "$ZSHRC" || cat << 'EOF' >> "$ZSHRC"

# Pganji Go Environment (No Root)
export GOPATH=$HOME/go
export GOMODCACHE=$HOME/go/pkg/mod
export GOCACHE=$HOME/.cache/go-build
export GOTOOLCHAIN=local

# Pganji Database & Run Aliases
alias start-db="pg_ctl -D $HOME/my_postgres -l $HOME/my_postgres/postgres.log start"
alias stop-db="pg_ctl -D $HOME/my_postgres stop"
alias run-pganji='DATABASE_URL="postgres:///pganji?host=$HOME/my_postgres/sockets&port=5433" go run main.go'
EOF
fi

# -----------------------------------------------------------------------------
# 4. go.mod & Dependency Alignment
# -----------------------------------------------------------------------------
cd "$PROJECT_DIR"
if [ -f "go.mod" ]; then
    CURRENT_GO_VER=$(go version | awk '{print $3}' | sed 's/go//')
    MOD_GO_VER=$(awk '/^go / {print $2}' go.mod)

    # Detect if go.mod requires a newer version than locally installed
    if [ "$CURRENT_GO_VER" != "$MOD_GO_VER" ]; then
        echo "[+] Aligning go.mod version ($MOD_GO_VER -> $CURRENT_GO_VER)..."
        sed -i "s/^go .*/go $CURRENT_GO_VER/" go.mod
        sed -i '/^toolchain /d' go.mod
        
        # Pin dependencies verified for Go 1.22+
        go get github.com/jackc/pgx/v5@v5.6.0
        go get github.com/go-chi/chi/v5@v5.0.12
        go get golang.org/x/time@v0.5.0
        go mod tidy
    fi
fi

# -----------------------------------------------------------------------------
# 5. Database Cluster Initialization & Custom Port/Socket Configuration
# -----------------------------------------------------------------------------
mkdir -p "$PG_SOCKETS"

if [ ! -f "$PG_DATA/PG_VERSION" ]; then
    echo "[+] Initializing new PostgreSQL cluster in $PG_DATA..."
    initdb -D "$PG_DATA"
    
    # Configure custom port and private socket path to avoid multi-user conflicts
    cat << EOF >> "$PG_DATA/postgresql.conf"
port = $PG_PORT
unix_socket_directories = '$PG_SOCKETS'
EOF
    echo "[✓] Cluster initialized and configured to port $PG_PORT."
else
    echo "[✓] PostgreSQL cluster already exists at $PG_DATA."
fi

# -----------------------------------------------------------------------------
# 6. Database Daemon Lifecycle
# -----------------------------------------------------------------------------
if ! pg_ctl -D "$PG_DATA" status &>/dev/null; then
    echo "[+] Starting PostgreSQL server daemon..."
    pg_ctl -D "$PG_DATA" -l "$PG_DATA/postgres.log" start
    sleep 2
else
    echo "[✓] PostgreSQL server daemon is already running."
fi

# -----------------------------------------------------------------------------
# 7. Database Creation & Schema Migration
# -----------------------------------------------------------------------------
if ! psql -p "$PG_PORT" -h "$PG_SOCKETS" -lqt | cut -d \| -f 1 | grep -qw pganji; then
    echo "[+] Creating 'pganji' database..."
    createdb -p "$PG_PORT" -h "$PG_SOCKETS" pganji
fi

if [ -f "$PROJECT_DIR/schema.sql" ]; then
    # Check if journal_entries table already exists before executing schema
    TABLE_CHECK=$(psql -p "$PG_PORT" -h "$PG_SOCKETS" -d pganji -tAc "SELECT to_regclass('public.journal_entries');")
    if [ "$TABLE_CHECK" != "journal_entries" ]; then
        echo "[+] Applying schema.sql migrations..."
        psql -p "$PG_PORT" -h "$PG_SOCKETS" -d pganji -f "$PROJECT_DIR/schema.sql"
        echo "[✓] Database schema successfully applied."
    else
        echo "[✓] Schema tables already exist in 'pganji' database."
    fi
fi

echo ""
echo "====================================================="
echo "           SETUP COMPLETED SUCCESSFULLY!             "
echo "====================================================="
echo "Database URL:"
echo "DATABASE_URL=\"postgres:///pganji?host=$PG_SOCKETS&port=$PG_PORT\""
echo ""
echo "To run your server now, run:"
echo "DATABASE_URL=\"postgres:///pganji?host=$PG_SOCKETS&port=$PG_PORT\" go run main.go"
echo ""
echo "Aliases added to ~/.zshrc (run 'source ~/.zshrc' to use them):"
echo "  start-db    -> Starts the background Postgres daemon"
echo "  stop-db     -> Stops the Postgres daemon safely"
echo "  run-pganji  -> Starts the Pganji server with database URL"
echo "====================================================="