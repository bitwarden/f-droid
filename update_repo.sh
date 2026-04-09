#!/bin/bash
set -e

# Check if the commit message file argument is provided
if [ "$#" -ne 1 ]; then
    echo "Usage: $0 <commit_message_file_path>"
    exit 1
fi

COMMIT_MSG_FILE="$1"

if [ ! -f "$COMMIT_MSG_FILE" ]; then
    echo "Error: Commit message file does not exist or could not be found: $COMMIT_MSG_FILE" >&2
    exit 1
fi

echo "Changes detected. Proceeding with git operations."

REPO="${GITHUB_REPOSITORY}"
BRANCH="update_fdroid_apps"
COMMIT_MSG=$(cat "$COMMIT_MSG_FILE")
PR_TITLE=$(head -n 1 "$COMMIT_MSG_FILE")
PR_BODY=$(tail -n +2 "$COMMIT_MSG_FILE")

echo "PR Title: $PR_TITLE"
echo "PR Body: $PR_BODY"

# Get the SHA of the base branch (main)
echo "Getting base branch SHA..."
BASE_SHA=$(gh api "repos/${REPO}/git/ref/heads/main" --jq '.object.sha')
BASE_TREE_SHA=$(gh api "repos/${REPO}/git/commits/${BASE_SHA}" --jq '.tree.sha')

# Build tree entries from changed/added/deleted files
echo "Building tree entries..."
TREE_ENTRIES="[]"

while IFS= read -r line; do
    STATUS="${line:0:2}"
    FILE="${line:3}"

    if [[ "$STATUS" == " D" || "$STATUS" == "D " ]]; then
        # Deleted file — null SHA removes it from the tree
        TREE_ENTRIES=$(echo "$TREE_ENTRIES" | jq \
            --arg path "$FILE" \
            '. + [{"path": $path, "mode": "100644", "type": "blob", "sha": null}]')
    else
        # Added or modified file — create a blob
        CONTENT=$(base64 -w 0 "$FILE")
        BLOB_SHA=$(gh api "repos/${REPO}/git/blobs" \
            --method POST \
            --field encoding=base64 \
            --field content="$CONTENT" \
            --jq '.sha')
        TREE_ENTRIES=$(echo "$TREE_ENTRIES" | jq \
            --arg path "$FILE" \
            --arg sha "$BLOB_SHA" \
            '. + [{"path": $path, "mode": "100644", "type": "blob", "sha": $sha}]')
    fi
done < <(git status --porcelain)

# Create new tree
echo "Creating tree..."
NEW_TREE_SHA=$(gh api "repos/${REPO}/git/trees" \
    --method POST \
    --field "base_tree=${BASE_TREE_SHA}" \
    --field "tree=$(echo "$TREE_ENTRIES")" \
    --jq '.sha')

# Create commit
echo "Creating commit..."
NEW_COMMIT_SHA=$(gh api "repos/${REPO}/git/commits" \
    --method POST \
    --field "message=${COMMIT_MSG}" \
    --field "tree=${NEW_TREE_SHA}" \
    --field "parents[]=${BASE_SHA}" \
    --jq '.sha')

# Force update (or create) the update_fdroid_apps branch ref
echo "Updating branch ref..."
if gh api "repos/${REPO}/git/ref/heads/${BRANCH}" > /dev/null 2>&1; then
    gh api "repos/${REPO}/git/refs/heads/${BRANCH}" \
        --method PATCH \
        --field sha="$NEW_COMMIT_SHA" \
        --field force=true
else
    gh api "repos/${REPO}/git/refs" \
        --method POST \
        --field ref="refs/heads/${BRANCH}" \
        --field sha="$NEW_COMMIT_SHA"
fi

# Create or update PR
echo "Checking for existing PR from branch ${BRANCH}..."
EXISTING_PR_NUMBER=$(gh pr list --head "$BRANCH" --json number --jq '.[0].number // empty')

if [ -n "$EXISTING_PR_NUMBER" ]; then
    echo "Existing PR found: #${EXISTING_PR_NUMBER}"
    gh pr edit "$EXISTING_PR_NUMBER" --title "$PR_TITLE" --body "$PR_BODY"
    echo "PR updated."
else
    echo "Creating PR..."
    gh pr create --title "$PR_TITLE" \
        --base main \
        --head "$BRANCH" \
        --label "automated pr" \
        --body "$PR_BODY"
fi

# Clean up
rm "$COMMIT_MSG_FILE"
