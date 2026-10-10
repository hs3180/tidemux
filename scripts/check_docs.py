#!/usr/bin/env python3
"""Check local Markdown destinations and GitHub heading anchors in source/package."""
import argparse
from collections import Counter
import html
from pathlib import Path
import re
import unicodedata
from urllib.parse import unquote, urlsplit


def visible_markdown(text):
    result = []
    fence = None
    for line in text.splitlines():
        match = re.match(r"\s*(`{3,}|~{3,})", line)
        if match:
            marker = match[1]
            if fence is None:
                fence = marker
            elif marker[0] == fence[0] and len(marker) >= len(fence):
                fence = None
            result.append("")
        else:
            result.append(line if fence is None else "")
    return re.sub(r"<!--.*?-->", "", "\n".join(result), flags=re.S)


def heading_anchors(text):
    anchors = set()
    counts = Counter()
    lines = visible_markdown(text).splitlines()
    for index, line in enumerate(lines):
        match = re.match(r"^\s{0,3}#{1,6}\s+(.+?)\s*#*\s*$", line)
        title = match[1] if match else None
        if title is None and index + 1 < len(lines) and re.match(r"^\s{0,3}(?:=+|-+)\s*$", lines[index + 1]) and line.strip():
            title = line.strip()
        if title is None:
            continue
        title = re.sub(r"!?\[([^\]]*)\]\([^)]*\)", r"\1", title)
        title = html.unescape(re.sub(r"<[^>]*>", "", title)).lower()
        slug = "".join(c for c in title if c in "-_ " or unicodedata.category(c)[0] in "LN")
        slug = slug.replace(" ", "-")
        occurrence = counts[slug]
        counts[slug] += 1
        anchors.add(slug if occurrence == 0 else f"{slug}-{occurrence}")
    anchors.update(re.findall(r'<a\s+(?:name|id)=["\']([^"\']+)', text, flags=re.I))
    return anchors


def destinations(text):
    text = visible_markdown(text)
    # Repository manuals use inline links and reference definitions. Balanced
    # parentheses are supported in paths; inline code/fenced examples are skipped.
    text = re.sub(r"`[^`\n]*`", "", text)
    for match in re.finditer(r"!?\[[^\]\n]*(?:\n[^\]]*)?\]\(", text):
        start, depth = match.end(), 1
        end = start
        while end < len(text) and depth:
            if text[end] == "(" and (end == 0 or text[end - 1] != "\\"):
                depth += 1
            elif text[end] == ")" and (end == 0 or text[end - 1] != "\\"):
                depth -= 1
            end += 1
        if depth == 0:
            target = text[start:end - 1].strip()
            if target.startswith("<"):
                target = target[1:target.index(">")]
            else:
                target = target.split(maxsplit=1)[0] if target else ""
            yield target
    for match in re.finditer(r"^\s{0,3}\[[^\]]+\]:\s*(<[^>]+>|\S+)", text, flags=re.M):
        yield match[1].strip("<>")


def check(root):
    errors, checked = [], 0
    files = sorted(p for p in root.rglob("*.md") if not any(part in {".git", "dist", "node_modules"} for part in p.relative_to(root).parts))
    anchor_cache = {}
    for source in files:
        for destination in destinations(source.read_text(encoding="utf-8")):
            if not destination or destination.startswith("//"):
                continue
            url = urlsplit(destination)
            if url.scheme:
                continue
            checked += 1
            target = (source.parent / unquote(url.path)).resolve() if url.path else source
            if target.is_dir():
                if not url.fragment:
                    continue
                target = target / "README.md"
            if not target.is_file():
                errors.append(f"{source.relative_to(root)}: missing {destination}")
                continue
            if url.fragment and target.suffix.lower() == ".md":
                if target not in anchor_cache:
                    anchor_cache[target] = heading_anchors(target.read_text(encoding="utf-8"))
                if unquote(url.fragment) not in anchor_cache[target]:
                    errors.append(f"{source.relative_to(root)}: missing anchor {destination}")
    return files, checked, errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    root = parser.parse_args().root.resolve()
    files, checked, errors = check(root)
    if errors:
        raise SystemExit("\n".join(errors))
    print(f"Checked {len(files)} Markdown files and {checked} local destinations/anchors")


if __name__ == "__main__":
    main()
