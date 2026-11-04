"""Finite, non-executable interaction metadata shared by all fixed actions."""

from .gateway import QueryError


def interaction(item, parameters):
    selected = item.get("interaction")
    for row in item.get("actions", []):
        defaults = row.get("parameters", {}) if isinstance(row, dict) else {}
        if defaults and all(parameters.get(k) == v for k, v in defaults.items()):
            selected = row.get("interaction")
            break
    if selected is None:
        # Compatibility with released servers which predate action metadata.
        if ("behaviorRevision" not in item or item.get("behaviorRevision") in {"1", "2", "3"}) and (
            item.get("id") == "workbench.act_nearby_entities"
            and parameters.get("action") in {"extinguish", "freeze", "sleep", "pacify", "delete", "kill"}
        ):
            return {"kind": "preview-confirm-v1", "resultVersion": 1, "effectVersion": "legacy"}
        return None
    if not supported(selected):
        raise QueryError("CLIENT_UPDATE_REQUIRED")
    return selected


def supported(value):
    return (isinstance(value, dict) and set(value) == {"kind", "resultVersion", "effectVersion"}
            and value.get("kind") == "preview-confirm-v1"
            and type(value.get("resultVersion")) is int and value["resultVersion"] in {1, 2}
            and isinstance(value.get("effectVersion"), str) and 1 <= len(value["effectVersion"]) <= 32)


def effective_risk(item, parameters=None):
    rows = item.get("actions", [])
    if parameters is not None:
        for row in rows:
            defaults = row.get("parameters", {}) if isinstance(row, dict) else {}
            if defaults and all(parameters.get(k) == v for k, v in defaults.items()):
                return row.get("risk", item.get("risk"))
        return item.get("risk")
    return item.get("risk")


def preview_seconds(batch, elapsed):
    version = batch.get("version", 1)
    if version == 1:
        seconds = 30
    elif version == 2:
        observed, expires = batch.get("observedAt"), batch.get("expiresAt")
        if type(observed) is not int or type(expires) is not int or expires - observed != 30:
            raise QueryError("MCP_RESPONSE_INVALID")
        seconds = expires - observed
    else:
        raise QueryError("CLIENT_UPDATE_REQUIRED")
    return max(0, seconds - max(0, elapsed))


def direct_request(value, items, actor):
    """A small whole-message grammar; ambiguity, conditions and discussion use AI."""
    import re
    from .capabilities import permitted
    from .routing import COMPOUND, DISCUSSION
    if len(value)>128 or COMPOUND.search(value) or DISCUSSION.search(value) or re.search(r"^(?:别|先不|暂不)|能不能|要不要|[？?]",value):
        return None
    capability,params=None,None
    match=re.fullmatch(r"(?:给|把|将)?(.{1,64}?)附近(?:的)?(?:作物|植物)?(施肥|催熟|推进生长|收获|采摘|拾取掉落物|拾取地面物品)",value)
    if match:
        action={"施肥":"fertilize","催熟":"ripen","推进生长":"grow","收获":"harvest","采摘":"harvest","拾取掉落物":"pick","拾取地面物品":"pick"}[match[2]]
        capability="workbench.act_nearby_entities"
        params={"player_id":match[1].strip(),"action":action,"radius":10}
    else:
        match=re.fullmatch(r"(?:把|将)?(.{1,64}?)(?:的)?饥饿消耗(?:改为|设为|调为)(一半|正常|两倍|零|0|0\.5|1|2)",value)
        if match:
            capability="workbench.set_player_hunger_rate"
            params={"player_id":match[1].strip(),"mode":"set","multiplier":{"一半":.5,"正常":1,"两倍":2,"零":0,"0":0,"0.5":.5,"1":1,"2":2}[match[2]]}
        else:
            match=re.fullmatch(r"(?:让|把|将)?(.{1,64}?)恢复人形",value)
            if match: capability="workbench.restore_woodie_human";params={"player_id":match[1].strip()}
    if not capability:
        modes={"无敌":"invincible","超级无敌":"super_god","制造":"creative","创造":"creative","锁血":"health_lock","一击必杀":"one_hit","隐身":"invisible","踏水":"water_walk","穿墙":"noclip"}
        match=re.fullmatch(r"(?:给|让|将)?(.{1,64}?)(?:的)?(开启|打开|启用|关闭|关掉|停用)(超级无敌|无敌|制造|创造|锁血|一击必杀|隐身|踏水|穿墙)(?:模式)?",value)
        if match:
            capability="workbench.set_player_mode";params={"player_id":match[1].strip(),"mode":modes[match[3]],"enabled":match[2] in {"开启","打开","启用"}}
        else:
            match=re.fullmatch(r"(?:给|让|将)?(.{1,64}?)(?:解除|关闭)全部(?:面板)?模式",value)
            if match:capability="workbench.set_player_mode";params={"player_id":match[1].strip(),"mode":"clear_all","enabled":False}
    if not capability:
        return None
    item=next((item for item in items if item.get("id")==capability),None)
    if not item or not permitted(actor,item,params) or not interaction(item,params):
        return None
    return capability,params
