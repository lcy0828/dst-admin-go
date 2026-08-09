import json
import math
import os

from lupa import LuaRuntime, lua_type


modinfo_path = os.environ["DST_MODINFO_PATH"]
mod_id = os.environ.get("DST_MODINFO_ID", "")
selected_locale = os.environ.get("DST_MODINFO_LOCALE", "zh")

lua = LuaRuntime(unpack_returned_tuples=True)
globals_table = lua.globals()
original_globals = {str(key) for key in globals_table}

globals_table.locale = selected_locale
globals_table.folder_name = "workshop-" + mod_id
lua.execute(
    "ChooseTranslationTable = function(translations) "
    "return translations[locale] or translations.zhr or translations[1] end"
)
globals_table.modimport = globals_table.require
globals_table.print = lambda *args: None

lua_path = os.environ.get("LUA_PATH")
lua_cpath = os.environ.get("LUA_CPATH")
if lua_path:
    globals_table.package.path = lua_path.replace(";;", ";" + str(globals_table.package.path) + ";", 1)
if lua_cpath:
    globals_table.package.cpath = lua_cpath.replace(";;", ";" + str(globals_table.package.cpath) + ";", 1)

with open(modinfo_path, "rb") as source_file:
    source = source_file.read().decode("utf-8-sig")
lua.execute(source)


def convert(value, visiting, depth):
    value_type = lua_type(value)
    if value_type == "function":
        return None
    if value_type != "table":
        if isinstance(value, float) and not math.isfinite(value):
            return None
        return value
    if depth > 100:
        raise ValueError("Lua table nesting exceeds 100 levels")
    identity = id(value)
    if identity in visiting:
        raise ValueError("cyclic Lua table")
    visiting.add(identity)
    try:
        keys = list(value.keys())
        if all(isinstance(key, int) and not isinstance(key, bool) for key in keys):
            ordered = sorted(keys)
            if ordered == list(range(1, len(ordered) + 1)):
                return [convert(value[index], visiting, depth + 1) for index in ordered]
        result = {}
        for key in keys:
            if isinstance(key, bool):
                mapped_key = "true" if key else "false"
            elif isinstance(key, (str, int)):
                mapped_key = str(key)
            elif isinstance(key, float) and math.isfinite(key):
                mapped_key = str(key)
            else:
                raise TypeError("unsupported Lua table key type")
            if mapped_key in result:
                raise ValueError("duplicate JSON key after Lua key conversion: " + mapped_key)
            result[mapped_key] = convert(value[key], visiting, depth + 1)
        return result
    finally:
        visiting.remove(identity)


excluded = {"locale", "folder_name", "ChooseTranslationTable", "modimport"}
result = {}
for key in globals_table:
    name = str(key)
    value = globals_table[key]
    if name not in original_globals and name not in excluded and lua_type(value) != "function":
        result[name] = convert(value, set(), 0)

print(json.dumps(result, ensure_ascii=False, allow_nan=False, separators=(",", ":")))
