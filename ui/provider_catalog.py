"""Conservative service-brand grouping of current regional TMDB providers.

Storefront variants and explicitly known plans share one picker entry. Every
source identity remains available by media type for discovery and old links.
"""
from __future__ import annotations

import math
import re
import unicodedata


_STOREFRONT_SUFFIX = re.compile(
    r"\s+(?:amazon(?: prime video)?|apple tv|roku(?: premium)?) channels?$", re.IGNORECASE)
_SAFE_LOGO = re.compile(
    r"https://image\.tmdb\.org/t/p/(?:w\d+|original)/[a-zA-Z\d_-]+\.(?:jpg|png|webp|svg)")


def _normal(name: str) -> str:
    return " ".join(unicodedata.normalize("NFKC", name).split()).casefold()


# Exact service/plan aliases only: Plus, Premium, Free, Kids and Ads are never
# removed generically, since those words can identify a separate catalogue.
_FAMILIES = {
    "Paramount+": ("Paramount+", "Paramount Plus", "Paramount Plus Premium", "Paramount+ Premium",
                   "Paramount Plus Essential", "Paramount+ Essential"),
    "Netflix": ("Netflix", "Netflix Kids", "Netflix Standard with Ads"),
    "Amazon Prime Video": ("Amazon Prime Video", "Amazon Prime Video Free with Ads",
                           "Amazon Prime Video with Ads"),
    "Peacock": ("Peacock", "Peacock Premium", "Peacock Premium Plus"),
    "Apple TV": ("Apple TV", "Apple TV+"),
    "HBO Max": ("HBO Max", "Max"),
    "AMC+": ("AMC+", "AMC Plus"),
    "MGM+": ("MGM+", "MGM Plus"),
    "discovery+": ("Discovery+", "Discovery +"),
    "CuriosityStream": ("CuriosityStream", "Curiosity Stream"),
    "Acorn TV": ("Acorn TV", "AcornTV"),
    "BritBox": ("BritBox",),
}
_ALIASES = {_normal(alias): canonical for canonical, aliases in _FAMILIES.items() for alias in aliases}
_PLAN_RANK = {
    _normal(name): rank for name, rank in (
        ("Paramount Plus", 0), ("Paramount+", 0),
        ("Paramount Plus Premium", 1), ("Paramount+ Premium", 1),
        ("Paramount Plus Essential", 2), ("Paramount+ Essential", 2),
        ("Peacock Premium", 1), ("Peacock Premium Plus", 2),
    )
}


def _priority(row: dict) -> int | float:
    value = row.get("displayPriority")
    return value if isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value) else 999


def _logo(value) -> str | None:
    return value if isinstance(value, str) and _SAFE_LOGO.fullmatch(value) else None


def group_providers(rows: list[dict]) -> list[dict]:
    """Group one regional catalogue without manufacturing IDs or availability."""
    families: dict[str, list[dict]] = {}
    for row in rows:
        if not isinstance(row, dict):
            continue
        identifier, name = row.get("id"), row.get("name")
        if (not isinstance(identifier, int) or isinstance(identifier, bool)
                or not 0 < identifier <= 2_147_483_647 or not isinstance(name, str) or not name.strip()):
            continue
        name = name.strip()[:200]
        display = " ".join(name.split())
        # This is a standalone service, rather than a distributor suffix.
        base = display if _normal(display) == "the roku channel" else _STOREFRONT_SUFFIX.sub("", display).strip()
        normalized = _normal(base)
        canonical = _ALIASES.get(normalized)
        key = _normal(canonical) if canonical else normalized
        families.setdefault(key, []).append({
            "row": row, "id": identifier, "rawName": name, "base": base,
            "canonical": canonical, "storefront": base != display,
            "planRank": _PLAN_RANK.get(normalized, 0 if normalized == key else 1),
        })

    result = []
    for brand_key, members in families.items():
        members.sort(key=lambda member: (
            member["storefront"], member["planRank"], not bool(_logo(member["row"].get("logo"))),
            _priority(member["row"]), member["id"], member["rawName"].casefold()))
        representative = members[0]
        assets = []
        # Prefer genuine direct-service primary logos, then direct alternatives;
        # distributor branding is a fallback when direct metadata has no image.
        for storefront in (False, True):
            subset = [member for member in members if member["storefront"] == storefront]
            for alternatives in (False, True):
                for member in subset:
                    row = member["row"]
                    candidates = row.get("logoAlternatives", []) if alternatives else [row.get("logo")]
                    for candidate in candidates if isinstance(candidates, list) else []:
                        asset = _logo(candidate)
                        if asset and asset not in assets:
                            assets.append(asset)
        ids_by_type = {
            media_type: sorted({member["id"] for member in members
                                if media_type in (member["row"].get("types") or [])})
            for media_type in ("movie", "tv_show")
        }
        aliases = {member["rawName"] for member in members}
        for member in members:
            raw_aliases = member["row"].get("aliases")
            if isinstance(raw_aliases, list):
                aliases.update(name.strip()[:200] for name in raw_aliases if isinstance(name, str) and name.strip())
        result.append({
            "id": representative["id"],
            "brandKey": brand_key,
            "name": representative["canonical"] or representative["base"],
            "logo": assets[0] if assets else None, "logoAlternatives": assets[1:],
            "displayPriority": min(_priority(member["row"]) for member in members),
            "types": [media_type for media_type, ids in ids_by_type.items() if ids],
            "providerIds": sorted({member["id"] for member in members}),
            "idsByType": ids_by_type,
            "aliases": sorted(aliases, key=lambda name: (name.casefold(), name)),
        })
    return sorted(result, key=lambda row: (row["displayPriority"], row["name"].casefold(), row["id"]))
