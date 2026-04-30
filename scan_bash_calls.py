"""
Scan Claude Code transcripts for Bash command usage patterns.

This development utility analyzes JSONL conversation transcripts from ~/.claude/projects/
to identify the most frequently used Bash commands. It parses assistant tool_use calls
for the Bash tool, extracts command patterns, and outputs a frequency table.

Use cases:
- Inform permission allowlist decisions (what commands to auto-allow)
- Understand agent behavior patterns
- Audit which commands are being run most often

Usage:
    python scan_bash_calls.py

Output:
    Frequency table of the top 40 most common command patterns, including both
    bare commands (e.g., "git") and command+subcommand pairs (e.g., "git status").
"""

import os
import json
import re
from collections import Counter

projects_dir = "/root/.claude/projects/"

# Find all .jsonl files
jsonl_files = []
for root, dirs, files in os.walk(projects_dir):
    for f in files:
        if f.endswith(".jsonl"):
            fp = os.path.join(root, f)
            jsonl_files.append((os.path.getmtime(fp), fp))

# Sort by modification time descending, take top 50
jsonl_files.sort(reverse=True)
top50 = [fp for _, fp in jsonl_files[:50]]

print(f"Found {len(jsonl_files)} total JSONL files, using top {len(top50)}", flush=True)

def extract_leading_token(cmd):
    """Extract the leading command token, handling env vars, sudo, timeout, pipes, &&, semicolons."""
    if not cmd or not cmd.strip():
        return None

    cmd = cmd.strip()

    # Split on newlines, take first meaningful line
    lines = [l.strip() for l in cmd.split('\n') if l.strip()]
    if not lines:
        return None
    cmd = lines[0]

    # Remove shell variable assignments at start (e.g. VAR=val cmd ...)
    # Keep consuming VAR=val patterns until we hit a real command
    tokens = cmd.split()
    idx = 0
    while idx < len(tokens) and re.match(r'^[A-Za-z_][A-Za-z0-9_]*=', tokens[idx]):
        idx += 1

    if idx >= len(tokens):
        return None

    first_token = tokens[idx]

    # Handle sudo / timeout / env / nice / nohup / time wrappers
    wrappers = {'sudo', 'timeout', 'env', 'nice', 'nohup', 'time', 'xargs', 'watch', 'strace', 'ltrace', 'unbuffered'}

    # Unwrap leading wrapper commands
    while first_token in wrappers and idx + 1 < len(tokens):
        idx += 1
        # Skip flags for sudo/timeout (e.g. sudo -u user, timeout 30)
        while idx < len(tokens) and tokens[idx].startswith('-'):
            idx += 1
        # Skip numeric argument for timeout
        if first_token == 'timeout' and idx < len(tokens) and re.match(r'^\d+', tokens[idx]):
            idx += 1
        if idx < len(tokens):
            first_token = tokens[idx]
        else:
            break

    # Strip path prefix (e.g. /usr/bin/git -> git)
    first_token = os.path.basename(first_token)

    # Remove any trailing characters that aren't part of a command name
    first_token = re.sub(r'[^a-zA-Z0-9_\-\.]', '', first_token)

    return first_token if first_token else None


def extract_command_subcommand(cmd):
    """Extract command + first subcommand pair for relevant commands."""
    if not cmd or not cmd.strip():
        return None, None

    cmd = cmd.strip()
    lines = [l.strip() for l in cmd.split('\n') if l.strip()]
    if not lines:
        return None, None
    cmd_line = lines[0]

    tokens = cmd_line.split()
    idx = 0

    # Skip env var assignments
    while idx < len(tokens) and re.match(r'^[A-Za-z_][A-Za-z0-9_]*=', tokens[idx]):
        idx += 1

    if idx >= len(tokens):
        return None, None

    first_token = tokens[idx]
    wrappers = {'sudo', 'timeout', 'env', 'nice', 'nohup', 'time', 'xargs', 'watch', 'strace', 'ltrace', 'unbuffered'}

    while first_token in wrappers and idx + 1 < len(tokens):
        idx += 1
        while idx < len(tokens) and tokens[idx].startswith('-'):
            idx += 1
        if first_token == 'timeout' and idx < len(tokens) and re.match(r'^\d+', tokens[idx]):
            idx += 1
        if idx < len(tokens):
            first_token = tokens[idx]
        else:
            break

    first_token = os.path.basename(first_token)
    first_token = re.sub(r'[^a-zA-Z0-9_\-\.]', '', first_token)

    if not first_token:
        return None, None

    # Commands where subcommand matters
    subcommand_cmds = {
        'git', 'docker', 'gh', 'npm', 'yarn', 'pip', 'pip3', 'python', 'python3',
        'kubectl', 'helm', 'terraform', 'aws', 'gcloud', 'az', 'cargo', 'go',
        'systemctl', 'service', 'apt', 'apt-get', 'yum', 'brew', 'snap',
        'make', 'mvn', 'gradle', 'pnpm', 'bun', 'deno', 'node', 'npx',
        'heroku', 'fly', 'vercel', 'netlify', 'stripe', 'firebase',
    }

    subcommand = None
    if first_token in subcommand_cmds and idx + 1 < len(tokens):
        next_idx = idx + 1
        # Skip flags
        while next_idx < len(tokens) and tokens[next_idx].startswith('-'):
            next_idx += 1
        if next_idx < len(tokens):
            sub = tokens[next_idx]
            # Only use as subcommand if it looks like a subcommand (not a file path or flag)
            if not sub.startswith('/') and not sub.startswith('-') and not re.match(r'^\d', sub):
                subcommand = sub

    return first_token, subcommand


# Counters
leading_token_counter = Counter()
cmd_subcmd_counter = Counter()
total_bash_calls = 0
files_processed = 0

for filepath in top50:
    try:
        with open(filepath, 'r', encoding='utf-8', errors='replace') as fh:
            for line in fh:
                line = line.strip()
                if not line:
                    continue
                try:
                    obj = json.loads(line)
                except json.JSONDecodeError:
                    continue

                # Look for assistant messages
                msg = obj.get('message', {})
                if not msg:
                    # Some formats have the message at top level
                    msg = obj

                role = msg.get('role', '')
                if role != 'assistant':
                    continue

                content = msg.get('content', [])
                if not isinstance(content, list):
                    continue

                for item in content:
                    if not isinstance(item, dict):
                        continue
                    if item.get('type') == 'tool_use' and item.get('name') == 'Bash':
                        inp = item.get('input', {})
                        command = inp.get('command', '')
                        if command:
                            total_bash_calls += 1

                            token = extract_leading_token(command)
                            if token:
                                leading_token_counter[token] += 1

                            cmd, sub = extract_command_subcommand(command)
                            if cmd and sub:
                                cmd_subcmd_counter[f"{cmd} {sub}"] += 1
                            elif cmd:
                                # Still count standalone commands in subcmd counter
                                pass
        files_processed += 1
    except Exception as e:
        print(f"Error processing {filepath}: {e}")

print(f"\nProcessed {files_processed} files, found {total_bash_calls} total Bash calls\n")

# Merge counters: for cmd+subcommand pairs, also show standalone if no subcommand tracked
# Build unified frequency table
unified = Counter()

# Add leading tokens
for tok, cnt in leading_token_counter.items():
    unified[tok] += cnt

# Add cmd+subcmd pairs (these are additional detail)
for pair, cnt in cmd_subcmd_counter.items():
    unified[pair] += cnt

# But we want to show both: leading tokens AND cmd+subcmd
# Let's show them merged by: if a cmd has subcommands, subtract subcommand counts from bare cmd count
# Actually, let's just present both as separate rows merged and sorted

# Build a combined table where cmd+subcmd entries are separate from bare cmd
# The bare cmd count is the total minus the subcommand breakdown
combined = Counter()

# Start with cmd+subcommand pairs
for pair, cnt in cmd_subcmd_counter.items():
    combined[pair] = cnt

# For leading tokens, add only the portion NOT covered by subcommand pairs
for tok, total_cnt in leading_token_counter.items():
    subcmd_total = sum(v for k, v in cmd_subcmd_counter.items() if k.startswith(tok + ' '))
    remainder = total_cnt - subcmd_total
    if remainder > 0:
        combined[tok] = remainder
    elif tok not in combined:
        combined[tok] = 0

print("=" * 50)
print("FREQUENCY TABLE (COUNT COMMAND_PATTERN)")
print("=" * 50)
for pattern, count in combined.most_common(40):
    print(f"{count:6d}  {pattern}")
