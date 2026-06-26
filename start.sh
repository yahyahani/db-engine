#!/bin/sh
# Builds and starts the db-engine dashboard in Docker, then prints
# the actual host port that Docker assigned.
set -e

# Create the data directory if it does not exist yet.
mkdir -p data

echo "Building and starting db-engine dashboard…"
docker compose up -d --build

# Give the container a moment to start.
sleep 1

# Ask Docker which host port maps to container port 8080.
PORT=$(docker compose port dashboard 8080 2>/dev/null | cut -d: -f2)

if [ -z "$PORT" ]; then
  echo ""
  echo "Container started but could not determine the port."
  echo "Check: docker compose logs dashboard"
  exit 1
fi

echo ""
echo "  ┌──────────────────────────────────────────┐"
echo "  │  db-engine dashboard                     │"
echo "  │  → http://localhost:${PORT}$(printf '%*s' $((26 - ${#PORT})) '')│"
echo "  │                                          │"
echo "  │  Database files: ./data/                 │"
echo "  │  Stop:  docker compose down              │"
echo "  │  Logs:  docker compose logs -f dashboard │"
echo "  └──────────────────────────────────────────┘"
echo ""
