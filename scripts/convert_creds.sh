#!/usr/bin/env bash
# Convert external credential JSON files to CLIProxyAPI-compatible format.
# Usage:
#   ./scripts/convert_creds.sh [--type gemini|antigravity] input_dir [output_dir]
#   ./scripts/convert_creds.sh [--type gemini|antigravity] single_file.json [output_dir]
#
# --type: Force credential type (gemini or antigravity). If omitted, will prompt per file.
# If output_dir is omitted, defaults to ./auths/
# Requires: jq

set -euo pipefail

if ! command -v jq &>/dev/null; then
  echo "Error: jq is required. Install with: brew install jq" >&2
  exit 1
fi

FORCE_TYPE=""

# Parse --type flag
while [[ $# -gt 0 ]]; do
  case "$1" in
    --type)
      FORCE_TYPE="$2"
      if [[ "$FORCE_TYPE" != "gemini" && "$FORCE_TYPE" != "antigravity" ]]; then
        echo "Error: --type must be 'gemini' or 'antigravity'" >&2
        exit 1
      fi
      shift 2
      ;;
    *)
      break
      ;;
  esac
done

INPUT="${1:?Usage: $0 [--type gemini|antigravity] <input_file_or_dir> [output_dir]}"
OUTPUT_DIR="${2:-./auths}"
mkdir -p "$OUTPUT_DIR"

CONVERTED=0
SKIPPED=0

convert_file() {
  local file="$1"
  local json
  json=$(cat "$file") || { echo "  SKIP: Cannot read $file"; SKIPPED=$((SKIPPED+1)); return; }

  # Check if it's valid JSON
  if ! echo "$json" | jq empty 2>/dev/null; then
    echo "  SKIP: Invalid JSON: $file"
    SKIPPED=$((SKIPPED+1))
    return
  fi

  # Check if already in CLIProxyAPI format (has "type" field)
  local existing_type
  existing_type=$(echo "$json" | jq -r '.type // empty')
  if [[ -n "$existing_type" ]]; then
    echo "  SKIP: Already has type=$existing_type: $file"
    SKIPPED=$((SKIPPED+1))
    return
  fi

  # Check if it's a Google OAuth credential
  local has_token_uri has_project_id has_scopes has_refresh_token
  has_token_uri=$(echo "$json" | jq -r '.token_uri // empty')
  has_project_id=$(echo "$json" | jq -r '.project_id // empty')
  has_scopes=$(echo "$json" | jq -r 'if .scopes then "yes" else "" end')
  has_refresh_token=$(echo "$json" | jq -r '.refresh_token // empty')

  if [[ -z "$has_refresh_token" ]]; then
    # Try nested token object
    has_refresh_token=$(echo "$json" | jq -r 'if (.token | type) == "object" then .token.refresh_token else "" end' 2>/dev/null || echo "")
  fi

  local is_google_oauth=false
  if [[ "$has_token_uri" == *"googleapis.com"* ]] || { [[ -n "$has_project_id" ]] && [[ -n "$has_scopes" ]]; }; then
    is_google_oauth=true
  fi

  if [[ "$is_google_oauth" != "true" ]]; then
    echo "  SKIP: Unrecognized credential format: $file"
    SKIPPED=$((SKIPPED+1))
    return
  fi

  # Determine target type
  local target_type="$FORCE_TYPE"
  if [[ -z "$target_type" ]]; then
    echo "  Google OAuth credential detected."
    echo "  Choose type: [1] gemini  [2] antigravity"
    read -rp "  Enter 1 or 2: " choice
    case "$choice" in
      1) target_type="gemini" ;;
      2) target_type="antigravity" ;;
      *)
        echo "  SKIP: Invalid choice"
        SKIPPED=$((SKIPPED+1))
        return
        ;;
    esac
  fi

  echo "  Converting as: $target_type"

  case "$target_type" in
    gemini)
      convert_gemini "$file" "$json"
      ;;
    antigravity)
      convert_antigravity "$file" "$json"
      ;;
  esac
}

# Extract common fields from various Google OAuth formats
extract_google_fields() {
  local json="$1"

  EMAIL=$(echo "$json" | jq -r '.user_email // .email // empty')
  PROJECT_ID=$(echo "$json" | jq -r '.project_id // empty')

  # Access token: could be top-level "token" (string) or nested "token.access_token" or top-level "access_token"
  ACCESS_TOKEN=$(echo "$json" | jq -r '
    if (.token | type) == "string" then .token
    elif (.token | type) == "object" then .token.access_token
    else .access_token // ""
    end
  ')

  # Refresh token: top-level or nested
  REFRESH_TOKEN=$(echo "$json" | jq -r '
    if (.token | type) == "object" then .token.refresh_token
    else .refresh_token // ""
    end
  ')

  EXPIRY=$(echo "$json" | jq -r '
    if (.token | type) == "object" then (.token.expiry // .expiry)
    else (.expiry // .expired // "")
    end
  ')
}

convert_gemini() {
  local file="$1"
  local json="$2"

  extract_google_fields "$json"

  # Client ID/Secret: top-level or nested, with default
  local client_id client_secret token_uri scopes
  client_id=$(echo "$json" | jq -r '
    if (.token | type) == "object" then (.token.client_id // .client_id)
    else (.client_id // "")
    end
  ')
  if [[ -z "$client_id" ]]; then
    client_id="681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com"
  fi

  client_secret=$(echo "$json" | jq -r '
    if (.token | type) == "object" then (.token.client_secret // .client_secret)
    else (.client_secret // "")
    end
  ')
  if [[ -z "$client_secret" ]]; then
    client_secret="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"
  fi

  token_uri=$(echo "$json" | jq -r '
    if (.token | type) == "object" then (.token.token_uri // .token_uri)
    else (.token_uri // "")
    end
  ')
  if [[ -z "$token_uri" ]]; then
    token_uri="https://oauth2.googleapis.com/token"
  fi

  scopes=$(echo "$json" | jq -c '
    if (.token | type) == "object" then (.token.scopes // .scopes // ["https://www.googleapis.com/auth/cloud-platform","https://www.googleapis.com/auth/userinfo.email","https://www.googleapis.com/auth/userinfo.profile"])
    else (.scopes // ["https://www.googleapis.com/auth/cloud-platform","https://www.googleapis.com/auth/userinfo.email","https://www.googleapis.com/auth/userinfo.profile"])
    end
  ')

  if [[ -z "$EMAIL" ]] || [[ -z "$REFRESH_TOKEN" ]]; then
    echo "  SKIP: Missing email or refresh_token in $file"
    SKIPPED=$((SKIPPED+1))
    return
  fi

  local output
  output=$(jq -n \
    --arg type "gemini" \
    --arg email "$EMAIL" \
    --arg project_id "$PROJECT_ID" \
    --arg access_token "$ACCESS_TOKEN" \
    --arg refresh_token "$REFRESH_TOKEN" \
    --arg client_id "$client_id" \
    --arg client_secret "$client_secret" \
    --arg token_uri "$token_uri" \
    --arg expiry "$EXPIRY" \
    --argjson scopes "$scopes" \
    '{
      type: $type,
      email: $email,
      project_id: $project_id,
      disabled: false,
      token: {
        access_token: $access_token,
        refresh_token: $refresh_token,
        client_id: $client_id,
        client_secret: $client_secret,
        token_uri: $token_uri,
        scopes: $scopes,
        expiry: $expiry,
        universe_domain: "googleapis.com"
      }
    }')

  local out_name="gemini-${EMAIL}-${PROJECT_ID}.json"
  local out_path="$OUTPUT_DIR/$out_name"

  if [[ -f "$out_path" ]]; then
    echo "  WARN: File already exists, overwriting: $out_name"
  fi

  echo "$output" > "$out_path"
  echo "  OK: → $out_name"
  CONVERTED=$((CONVERTED+1))
}

convert_antigravity() {
  local file="$1"
  local json="$2"

  extract_google_fields "$json"

  if [[ -z "$EMAIL" ]] || [[ -z "$REFRESH_TOKEN" ]]; then
    echo "  SKIP: Missing email or refresh_token in $file"
    SKIPPED=$((SKIPPED+1))
    return
  fi

  # Antigravity format is flat, similar to native CLIProxyAPI antigravity files
  local expires_in
  expires_in=$(echo "$json" | jq -r '.expires_in // 3599')

  local output
  output=$(jq -n \
    --arg access_token "$ACCESS_TOKEN" \
    --arg email "$EMAIL" \
    --arg expired "$EXPIRY" \
    --argjson expires_in "$expires_in" \
    --arg refresh_token "$REFRESH_TOKEN" \
    --argjson timestamp "$(date +%s000)" \
    '{
      access_token: $access_token,
      disabled: false,
      email: $email,
      expired: $expired,
      expires_in: $expires_in,
      refresh_token: $refresh_token,
      timestamp: $timestamp,
      type: "antigravity"
    }')

  local out_name="antigravity-${EMAIL}.json"
  local out_path="$OUTPUT_DIR/$out_name"

  if [[ -f "$out_path" ]]; then
    echo "  WARN: File already exists, overwriting: $out_name"
  fi

  echo "$output" > "$out_path"
  echo "  OK: → $out_name"
  CONVERTED=$((CONVERTED+1))
}

# Main
echo "Converting credentials..."
echo "Output directory: $OUTPUT_DIR"
if [[ -n "$FORCE_TYPE" ]]; then
  echo "Forced type: $FORCE_TYPE"
fi
echo ""

if [[ -d "$INPUT" ]]; then
  for f in "$INPUT"/*.json; do
    [[ -f "$f" ]] || continue
    echo "Processing: $(basename "$f")"
    convert_file "$f"
  done
else
  echo "Processing: $(basename "$INPUT")"
  convert_file "$INPUT"
fi

echo ""
echo "Done. Converted: $CONVERTED, Skipped: $SKIPPED"
