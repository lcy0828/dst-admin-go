"""Render README badges and star history from GitHub API data (standard library only)."""

import json
import math
import os
from collections import Counter
from datetime import datetime, timedelta, timezone
from html import escape
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import Request, urlopen


REPOSITORY_ROOT = Path(__file__).resolve().parents[2]
ASSET_DIRECTORY = REPOSITORY_ROOT / "docs/assets/readme/metrics"
GREEN = "#8ac496"
GOLD = "#e7b76e"


def github(path):
    request = Request(
        f"https://api.github.com/repos/{os.environ['GITHUB_REPOSITORY']}" + (f"/{path}" if path else ""),
        headers={
            "Authorization": f"Bearer {os.environ['GH_TOKEN']}",
            "Accept": "application/vnd.github.star+json",
            "X-GitHub-Api-Version": "2022-11-28",
            "User-Agent": "dst-admin-readme-metrics",
        },
    )
    with urlopen(request, timeout=30) as response:
        return json.load(response)


def badge(label, value, color):
    left, right = len(label) * 6 + 12, len(value) * 6 + 12
    title = escape(f"{label}: {value}")
    return f'''<svg xmlns="http://www.w3.org/2000/svg" width="{left + right}" height="28" role="img" aria-label="{title}">
  <title>{title}</title>
  <rect width="{left + right}" height="28" rx="6" fill="#20362a"/>
  <path d="M{left} 0h{right - 6}q6 0 6 6v16q0 6-6 6H{left}Z" fill="{color}"/>
  <g font-family="Verdana,DejaVu Sans,sans-serif" font-size="11" text-anchor="middle">
    <text x="{left / 2}" y="18" fill="#f4f3e9">{escape(label)}</text>
    <text x="{left + right / 2}" y="18" fill="#17251e" font-weight="bold">{escape(value)}</text>
  </g>
</svg>
'''


def workflow_badge(filename, branch):
    query = urlencode({"branch": branch, "event": "push", "per_page": 1})
    try:
        runs = github(f"actions/workflows/{filename}/runs?{query}").get("workflow_runs", [])
    except HTTPError as error:
        if error.code != 404:
            raise
        runs = []
    if not runs:
        return "no runs", "#b9c4b8"
    run = runs[0]
    if run["status"] != "completed":
        return "running", GOLD
    conclusion = run.get("conclusion") or "unknown"
    if conclusion == "success":
        return "passing", GREEN
    if conclusion in ("failure", "timed_out", "action_required", "startup_failure"):
        return "failing", "#ee9c87"
    return conclusion, "#b9c4b8"


def star_history(repository, dates, today, language):
    """Reconstruct the cumulative history of current stargazers, as Star History does."""
    counts = Counter(dates)
    created = datetime.fromisoformat(repository["created_at"].replace("Z", "+00:00")).date()
    start = min([created, today - timedelta(days=1), *counts])
    end = today
    total = sum(counts.values())
    star_label = "Star" if total == 1 else "Stars"
    upper = max(1, math.ceil(total / 4) * 4) if total else 1
    left, right, top, bottom = 76, 1138, 134, 310
    x = lambda day: left + (day - start).days / (end - start).days * (right - left)
    y = lambda value: bottom - value / upper * (bottom - top)
    points = [(left, bottom)]
    cumulative = 0
    for day, count in sorted(counts.items()):
        points.append((x(day), y(cumulative)))
        cumulative += count
        points.append((x(day), y(cumulative)))
    points.append((right, y(cumulative)))
    line = "M" + " L".join(f"{px:.1f},{py:.1f}" for px, py in points)
    area = line + f" L{right},{bottom} L{left},{bottom} Z"
    zh = language == "zh"
    title = "Star 趋势" if zh else "Star history"
    subtitle = "每一颗 Star，都是同行的冒险者。" if zh else "Every star is another adventurer along the way."
    note = "按当前 Star 用户的关注时间统计" if zh else "Based on current stargazers"
    updated = "更新" if zh else "Updated"
    horizontal = []
    for value in sorted({0, upper // 2, upper}):
        yy = y(value)
        horizontal.append(f'<path d="M{left} {yy:.1f}H{right}" stroke="#344638" stroke-dasharray="4 7"/>')
        horizontal.append(f'<text x="56" y="{yy + 5:.1f}" text-anchor="end">{value}</text>')
    axis = []
    for ratio, anchor in [(0, "start"), (0.5, "middle"), (1, "end")]:
        day = start + timedelta(days=round((end - start).days * ratio))
        axis.append(f'<text x="{x(day):.1f}" y="339" text-anchor="{anchor}">{day.isoformat()}</text>')
    return f'''<svg xmlns="http://www.w3.org/2000/svg" width="1200" height="410" viewBox="0 0 1200 410" role="img" aria-labelledby="title description">
  <title id="title">{title} · {total} {star_label}</title>
  <desc id="description">{note}. {start.isoformat()} — {today.isoformat()}. {total} {star_label}.</desc>
  <defs><linearGradient id="fill" x2="0" y2="1"><stop stop-color="{GOLD}" stop-opacity=".25"/><stop offset="1" stop-color="{GOLD}" stop-opacity=".02"/></linearGradient></defs>
  <rect x=".5" y=".5" width="1199" height="409" rx="18" fill="#17251e" stroke="#384c3e"/>
  <g font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',Arial,sans-serif">
    <text x="42" y="53" fill="#f5f1e6" font-size="25" font-weight="650">{title}</text>
    <text x="42" y="84" fill="#b9c4b8" font-size="16">{subtitle}</text>
    <text x="1158" y="58" text-anchor="end" fill="{GOLD}" font-size="32" font-weight="700">{total:,} <tspan font-size="16">{star_label}</tspan></text>
    <g fill="#b9c4b8" font-size="14">{''.join(horizontal)}{''.join(axis)}</g>
    <path d="{area}" fill="url(#fill)"/>
    <path d="{line}" fill="none" stroke="{GOLD}" stroke-width="3" stroke-linejoin="round"/>
    <circle cx="{right}" cy="{y(cumulative):.1f}" r="5" fill="{GOLD}" stroke="#17251e" stroke-width="3"/>
    <text x="42" y="384" fill="#b9c4b8" font-size="13">{note}</text>
    <text x="1158" y="384" text-anchor="end" fill="#b9c4b8" font-size="13">{updated} {today.isoformat()} UTC</text>
  </g>
</svg>
'''


def main():
    repository = github("")
    dates = []
    page = 1
    while True:
        stars = github(f"stargazers?per_page=100&page={page}")
        dates.extend(datetime.fromisoformat(star["starred_at"].replace("Z", "+00:00")).date() for star in stars)
        if len(stars) < 100:
            break
        page += 1
    # Avoid publishing a misleading graph if stars changed during pagination.
    if len(dates) != repository["stargazers_count"]:
        raise RuntimeError("Star count changed while fetching data; keeping existing assets. Run again.")
    assets = {
        "stars.svg": badge("Stars", str(repository["stargazers_count"]), GOLD),
        "forks.svg": badge("Forks", str(repository["forks_count"]), GREEN),
    }
    for name, workflow in [("ci", "ci.yml"), ("package", "package.yml")]:
        value, color = workflow_badge(workflow, repository["default_branch"])
        assets[f"{name}.svg"] = badge("CI" if name == "ci" else "Package", value, color)
    today = datetime.now(timezone.utc).date()
    for language in ["zh", "en"]:
        assets[f"stars.{language}.svg"] = star_history(repository, dates, today, language)
    ASSET_DIRECTORY.mkdir(parents=True, exist_ok=True)
    for filename, content in assets.items():
        (ASSET_DIRECTORY / filename).write_text(content, encoding="utf-8")
    print(f"Updated {len(assets)} README assets: {len(dates)} Stars, {repository['forks_count']} Forks")


if __name__ == "__main__":
    main()
