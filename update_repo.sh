#!/bin/bash
set -e

# Check if the commit message file argument is provided
if [ "$#" -ne 1 ]; then
    echo "Usage: $0 <commit_message_file_path>"
    exit 1
fi

COMMIT_MSG_FILE="$1"

if [ -f "$COMMIT_MSG_FILE" ]; then
    echo "Changes detected. Proceeding with git operations."

    # Read the first line as the PR title
    PR_TITLE=$(head -n 1 "$COMMIT_MSG_FILE")
    echo "PR Title: $PR_TITLE"

    # Read the remaining lines as the PR body
    PR_BODY=$(tail -n +2 "$COMMIT_MSG_FILE")
    echo "PR Body: $PR_BODY"

    echo "Checking for existing PR from branch update_fdroid_apps..."
    EXISTING_PR_JSON=$(gh pr list --head update_fdroid_apps --json number)
    EXISTING_PR_NUMBER=$(echo "$EXISTING_PR_JSON" | jq -r '.[0].number // empty')

    # Always push changes to update_fdroid_apps branch
    git add .
    git checkout -B update_fdroid_apps
    git commit -F "$COMMIT_MSG_FILE"
    git push -f -u origin update_fdroid_apps

    if [ -n "$EXISTING_PR_NUMBER" ]; then
        echo "Existing PR found: #$EXISTING_PR_NUMBER"
        # Update title and body of the existing PR
        gh pr edit "$EXISTING_PR_NUMBER" --title "$PR_TITLE" --body "$PR_BODY"
        echo "PR updated."
        echo "pr_number=$EXISTING_PR_NUMBER"
    else
        echo "Creating PR..."
        PR_URL=$(gh pr create --title "$PR_TITLE" \
            --base main \
            --label "automated pr" \
            --body "$PR_BODY")
        echo "pr_number=${PR_URL##*/}"
    fi

    # Clean up the temporary commit message file
    rm "$COMMIT_MSG_FILE"

else
    echo "Error: Commit message file does not exist or could not be found: $COMMIT_MSG_FILE" >&2
    exit 1
fi
