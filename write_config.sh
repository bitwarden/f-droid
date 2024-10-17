#!/bin/bash

# Capture keypass and keystorepass arguments
while [[ $# -gt 0 ]]; do
    case "$1" in
    -k|--keypass)
      keypass="$2"
      shift 2
      ;;
    -s|--keystorepass)
      keystorepass="$2"
      shift 2
      ;;
    *)
      echo "Unknown option: $1" >&2
      exit 1
      ;;
    esac
done;

# Check if keypass and keystorepass are provided
if [[ -z "$keypass" ]]; then
  echo "Error: --keypass or -k is required." >&2
  exit 1
fi
if [[ -z "$keystorepass" ]]; then
  echo "Error: --keystorepass or -s is required." >&2
  exit 1
fi

cat <<EOF > fdroid/config.yml
---
repo_url: 'https://mobileapp.bitwarden.com/fdroid/repo'
mirrors:
  - 'https://raw.githubusercontent.com/bitwarden/f-droid/main/fdroid'
repo_name: 'Bitwarden F-Droid'
repo_description: |
  This is a repository of Bitwarden apps to be used with F-Droid. Applications
  in this repository are official binaries built by Bitwarden.
archive_url: 'https://mobileapp.bitwarden.com/fdroid/archive'
archive_name: 'Bitwarden F-droid Archive'
archive_description: |
  This is a repository of archived Bitwarden apps that are no longer
  officially supported.
archive_older: 10
repo_keyalias: 'bitwarden-Virtual-Machine'
keydname: 'CN=bitwarden-Virtual-Machine, OU=F-Droid'
gpg_keyid: 'ggpkey'
email: android@bitwarden.com
EOF

chmod 0600 fdroid/config.yml

echo "keypass: '$keypass'" >> fdroid/config.yml
echo "keystorepass: '$keystorepass'" >> fdroid/config.yml
