local modinfo_path = assert(os.getenv("DST_MODINFO_PATH"), "DST_MODINFO_PATH is required")
local mod_id = os.getenv("DST_MODINFO_ID") or ""
local selected_locale = os.getenv("DST_MODINFO_LOCALE") or "zh"

local original_globals = {}
for key in pairs(_G) do
    original_globals[key] = true
end

locale = selected_locale
folder_name = "workshop-" .. mod_id
ChooseTranslationTable = function(translations)
    return translations[selected_locale] or translations.zhr or translations[1]
end
modimport = require
print = function() end

local chunk, load_error = loadfile(modinfo_path)
if not chunk then
    error(load_error)
end
chunk()

local escapes = {
    ['"'] = '\\"',
    ['\\'] = '\\\\',
    ['\b'] = '\\b',
    ['\f'] = '\\f',
    ['\n'] = '\\n',
    ['\r'] = '\\r',
    ['\t'] = '\\t',
}

local function encode_string(value)
    return '"' .. value:gsub('[%z\1-\31\\"]', function(character)
        return escapes[character] or string.format('\\u%04x', string.byte(character))
    end) .. '"'
end

local function table_shape(value)
    local count = 0
    local maximum = 0
    for key in pairs(value) do
        if type(key) ~= "number" or key < 1 or key ~= math.floor(key) then
            return false, 0
        end
        count = count + 1
        if key > maximum then
            maximum = key
        end
    end
    return count == maximum, maximum
end

local function encode(value, visiting, depth)
    local kind = type(value)
    if kind == "nil" then
        return "null"
    elseif kind == "boolean" then
        return value and "true" or "false"
    elseif kind == "number" then
        if value ~= value or value == math.huge or value == -math.huge then
            return "null"
        end
        return tostring(value)
    elseif kind == "string" then
        return encode_string(value)
    elseif kind == "function" then
        return "null"
    elseif kind ~= "table" then
        error("unsupported Lua value type: " .. kind)
    elseif depth > 100 then
        error("Lua table nesting exceeds 100 levels")
    end

    if visiting[value] then
        error("cyclic Lua table")
    end
    visiting[value] = true

    local parts = {}
    local is_array, length = table_shape(value)
    if is_array then
        for index = 1, length do
            parts[#parts + 1] = encode(value[index], visiting, depth + 1)
        end
        visiting[value] = nil
        return "[" .. table.concat(parts, ",") .. "]"
    end

    local mapped = {}
    for key, item in pairs(value) do
        local key_kind = type(key)
        if key_kind == "string" or key_kind == "number" or key_kind == "boolean" then
            if key_kind == "number" and (key ~= key or key == math.huge or key == -math.huge) then
                error("unsupported non-finite Lua table key")
            end
            local mapped_key = tostring(key)
            if mapped[mapped_key] ~= nil then
                error("duplicate JSON key after Lua key conversion: " .. mapped_key)
            end
            mapped[mapped_key] = item
        else
            error("unsupported Lua table key type: " .. key_kind)
        end
    end
    local keys = {}
    for key in pairs(mapped) do
        keys[#keys + 1] = key
    end
    table.sort(keys)
    for _, key in ipairs(keys) do
        parts[#parts + 1] = encode_string(key) .. ":" .. encode(mapped[key], visiting, depth + 1)
    end
    visiting[value] = nil
    return "{" .. table.concat(parts, ",") .. "}"
end

local excluded = {
    locale = true,
    folder_name = true,
    ChooseTranslationTable = true,
    modimport = true,
}
local result = {}
for key, value in pairs(_G) do
    if type(key) == "string" and not original_globals[key] and not excluded[key] and type(value) ~= "function" then
        result[key] = value
    end
end

io.write(encode(result, {}, 0))
