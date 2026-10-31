"""Source compatibility for the retired standalone broadcaster.

The package builder embeds the canonical engine for standalone legacy builds.
"""

try:
    from ..astrbot_plugin_dst_companion.broadcast import READ_TOOLS, BroadcastEngine
except ImportError:
    from astrbot_plugin_dst_companion.broadcast import READ_TOOLS, BroadcastEngine

__all__ = ["READ_TOOLS", "BroadcastEngine"]
