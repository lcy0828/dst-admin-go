"""Select existing MCP connections without guessing between different panels."""

from urllib.parse import urlsplit, urlunsplit

from .gateway import QueryError

DISCOVERY_TOOLS = frozenset(
    {"api_listRooms", "api_listRoomWorlds", "api_listWorldStates", "api_listRoomPlayers"}
)


def endpoint(config):
    try:
        url = urlsplit(str((config or {}).get("url", "")))
        if url.scheme not in {"http", "https"} or not url.hostname or url.username or url.password:
            return ""
        port = url.port or (443 if url.scheme == "https" else 80)
        host = url.hostname.lower()
        path = url.path.rstrip("/")
        return urlunsplit((url.scheme, f"{host}:{port}", path, "", ""))
    except ValueError:
        return ""


class Connections:
    def __init__(self, manager, config):
        self.manager, self.config = manager, config
        self.selected = ""

    def configuration(self, name):
        runtime = self.manager.mcp_server_runtime_view.get(name)
        value = getattr(getattr(runtime, "client", None), "_mcp_server_config", None)
        if isinstance(value, dict):
            return value
        load = getattr(self.manager, "load_mcp_config", None)
        return load().get("mcpServers", {}).get(name, {}) if load else {}

    def names(self, required=DISCOVERY_TOOLS):
        return sorted(
            name
            for name, runtime in self.manager.mcp_server_runtime_view.items()
            if getattr(runtime.client, "session", None) is not None
            and self.configuration(name).get("active", True) is not False
            and required <= {t.name for t in runtime.client.tools}
        )

    def read(self):
        explicit = str(self.config.get("mcp_server", "")).strip()
        if explicit:
            return explicit  # An unavailable explicit source must never fall back to another panel.
        if self.selected:
            return self.selected
        candidates = self.names()
        if len(candidates) != 1:
            raise QueryError("MCP_SELECTION_REQUIRED" if candidates else "PANEL_DISCONNECTED")
        self.selected = candidates[0]
        return self.selected

    def source(self):
        name = self.read()
        # Credentials are deliberately excluded: rotating a key refreshes the
        # catalog but does not silently move a group's binding to another panel.
        return name + "|" + endpoint(self.configuration(name))

    def broadcast(self, required):
        source = self.read()
        advanced = self.config.get("advanced", {})
        if not isinstance(advanced, dict):
            return ""
        explicit = advanced.get("broadcastConnection", "")
        origin = endpoint(self.configuration(source))
        candidates = [
            n for n in self.names(required) if origin and endpoint(self.configuration(n)) == origin
        ]
        if explicit:
            return explicit if explicit in candidates else ""
        if source in candidates:
            return source
        management = str(self.config.get("management_mcp_server") or "")
        if management in candidates:
            return management  # Explicitly selected, same-panel, permitted connection.
        return candidates[0] if len(candidates) == 1 else ""
