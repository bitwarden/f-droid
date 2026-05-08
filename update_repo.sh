#!/bin/bash
set -e

if [ "$#" -ne 1 ]; then
    echo "Usage: $0 <commit_message_file_path>"
    exit 1
fi

COMMIT_MSG_FILE="$1"

if [ ! -f "$COMMIT_MSG_FILE" ]; then
    echo "Error: Commit message file does not exist: $COMMIT_MSG_FILE" >&2
    exit 1
fi

echo "Changes detected. Proceeding with git operations."

REPO="${GITHUB_REPOSITORY}"
BRANCH="update_fdroid_apps"
COMMIT_MSG=$(cat "$COMMIT_MSG_FILE")
PR_TITLE=$(head -n 1 "$COMMIT_MSG_FILE")
PR_BODY=$(tail -n +2 "$COMMIT_MSG_FILE")

echo "PR Title: $PR_TITLE"

# --- Stage and push via git transport (no API size limits) ---
echo "Creating local branch and committing..."
git checkout -B "$BRANCH"
git add -A
git -c user.name="bw-ghapp[bot]" \
    -c user.email="178206702+bw-ghapp[bot]@users.noreply.github.com" \
    commit -m "$COMMIT_MSG"

echo "Pushing branch to GitHub..."
git push --force origin "$BRANCH"

# --- Replace the unverified commit with a verified API commit ---
# The tree is already on GitHub from the push above. We just create a new
# commit object pointing at that tree, parented on current main, signed by
# the App via the API path. Then point the branch ref at it.

echo "Resolving tree SHA from pushed commit..."
LOCAL_COMMIT_SHA=$(git rev-parse HEAD)
TREE_SHA=$(gh api "repos/${REPO}/git/commits/${LOCAL_COMMIT_SHA}" --jq '.tree.sha')

echo "Resolving base (main) SHA..."
BASE_SHA=$(gh api "repos/${REPO}/git/ref/heads/main" --jq '.object.sha')

echo "Creating verified commit via API..."
VERIFIED_COMMIT_SHA=$(jq -n \
    --arg msg "$COMMIT_MSG" \
    --arg tree "$TREE_SHA" \
    --arg parent "$BASE_SHA" \
    '{message: $msg, tree: $tree, parents: [$parent]}' \
  | gh api "repos/${REPO}/git/commits" \
        --method POST \
        --input - \
        --jq '.sha')

echo "Updating branch ref to verified commit: $VERIFIED_COMMIT_SHA"
gh api "repos/${REPO}/git/refs/heads/${BRANCH}" \
    --method PATCH \
    --field sha="$VERIFIED_COMMIT_SHA" \
    --field force=true

# --- Create or update PR ---
echo "Checking for existing PR..."
EXISTING_PR_NUMBER=$(gh pr list --head "$BRANCH" --base main --json number --jq '.[0].number // empty')

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

rm "$COMMIT_MSG_FILE"
