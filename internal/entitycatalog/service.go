package entitycatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var ErrInvalidSearch = errors.New("entity catalog search is invalid")

var prefabPattern = regexp.MustCompile(`^[a-z0-9_]{1,80}$`)

const maxCatalogResults = 120

type Capability string

const (
	CapabilityGive   Capability = "give"
	CapabilitySpawn  Capability = "spawn"
	CapabilityRemove Capability = "remove"
)

type Entity struct {
	Key          string       `json:"key"`
	Namespace    string       `json:"namespace"`
	ID           string       `json:"id"`
	NameZhCN     string       `json:"nameZhCN"`
	NameEn       string       `json:"nameEn"`
	Type         string       `json:"type"`
	ArtworkURL   string       `json:"artworkUrl,omitempty"`
	Capabilities []Capability `json:"capabilities"`
	Common       bool         `json:"common"`
	Source       string       `json:"source"`
}

type SearchOptions struct {
	Query string
	Kind  Capability
	Limit int
}

type SearchResult struct {
	Items           []Entity `json:"items"`
	Query           string   `json:"query"`
	Source          string   `json:"source"`
	RemoteAvailable bool     `json:"remoteAvailable"`
	ReleaseID       string   `json:"releaseId,omitempty"`
}

type Service struct {
	baseURL *url.URL
	client  *http.Client
}

func New(baseURL string, client *http.Client) (*Service, error) {
	baseURL = strings.TrimSpace(baseURL)
	var parsed *url.URL
	if baseURL != "" {
		value, err := url.Parse(baseURL)
		if err != nil || value.Scheme != "http" && value.Scheme != "https" || value.Host == "" || value.User != nil || value.RawQuery != "" || value.Fragment != "" {
			return nil, fmt.Errorf("%w: beacon URL", ErrInvalidSearch)
		}
		value.Path = strings.TrimRight(value.Path, "/")
		parsed = value
	}
	if client == nil {
		client = &http.Client{Timeout: 2500 * time.Millisecond}
	}
	return &Service{baseURL: parsed, client: client}, nil
}

func (s *Service) Search(ctx context.Context, options SearchOptions) (SearchResult, error) {
	query := strings.TrimSpace(options.Query)
	if len([]rune(query)) > 100 || options.Limit < 1 || options.Limit > maxCatalogResults ||
		options.Kind != "" && options.Kind != CapabilityGive && options.Kind != CapabilitySpawn && options.Kind != CapabilityRemove {
		return SearchResult{}, ErrInvalidSearch
	}

	local := searchCommon(query, options.Kind)
	result := SearchResult{Items: local, Query: query, Source: "builtin", RemoteAvailable: false}
	if query == "" || s.baseURL == nil {
		result.Items = limitEntities(result.Items, options.Limit)
		return result, nil
	}

	remote, releaseID, err := s.searchBeacon(ctx, query, options)
	if err != nil {
		result.Items = limitEntities(result.Items, options.Limit)
		return result, nil
	}
	result.Items = mergeEntities(local, remote, options.Limit)
	result.Source = "beacon"
	result.RemoteAvailable = true
	result.ReleaseID = releaseID
	return result, nil
}

type beaconResponse struct {
	Data []struct {
		Key           string `json:"key"`
		Namespace     string `json:"namespace"`
		ID            string `json:"id"`
		Type          string `json:"type"`
		CatalogStatus string `json:"catalogStatus"`
		Names         struct {
			ZhCN string `json:"zhCN"`
			En   string `json:"en"`
		} `json:"names"`
		Components []string `json:"components"`
		Artwork    *struct {
			Path string `json:"path"`
		} `json:"artwork"`
	} `json:"data"`
	Meta struct {
		Release struct {
			ID string `json:"id"`
		} `json:"release"`
	} `json:"meta"`
}

func (s *Service) searchBeacon(ctx context.Context, query string, options SearchOptions) ([]Entity, string, error) {
	endpoint := *s.baseURL
	endpoint.Path += "/api/v1/search"
	parameters := endpoint.Query()
	parameters.Set("q", query)
	parameters.Set("status", "published")
	parameters.Set("limit", "20")
	endpoint.RawQuery = parameters.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Accept", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("beacon search returned HTTP %d", response.StatusCode)
	}

	var payload beaconResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if err := decoder.Decode(&payload); err != nil {
		return nil, "", err
	}
	items := make([]Entity, 0, len(payload.Data))
	for _, item := range payload.Data {
		if !prefabPattern.MatchString(item.ID) || item.Namespace == "" || item.CatalogStatus != "published" {
			continue
		}
		capabilities := []Capability{CapabilitySpawn, CapabilityRemove}
		if contains(item.Components, "inventoryitem") {
			capabilities = append([]Capability{CapabilityGive}, capabilities...)
		}
		if options.Kind != "" && !containsCapability(capabilities, options.Kind) {
			continue
		}
		artworkURL := ""
		if item.Artwork != nil && strings.HasPrefix(item.Artwork.Path, "/assets/") {
			artwork := s.baseURL.ResolveReference(&url.URL{Path: item.Artwork.Path})
			artworkURL = artwork.String()
		}
		items = append(items, Entity{
			Key: item.Key, Namespace: item.Namespace, ID: item.ID,
			NameZhCN: item.Names.ZhCN, NameEn: item.Names.En, Type: item.Type,
			ArtworkURL: artworkURL, Capabilities: capabilities, Source: "beacon",
		})
	}
	return items, payload.Meta.Release.ID, nil
}

func searchCommon(query string, kind Capability) []Entity {
	query = strings.ToLower(strings.TrimSpace(query))
	items := make([]Entity, 0, len(commonEntities))
	for _, item := range commonEntities {
		if kind != "" && !containsCapability(item.Capabilities, kind) {
			continue
		}
		body := strings.ToLower(strings.Join([]string{item.ID, item.NameZhCN, item.NameEn}, " "))
		if query != "" && !strings.Contains(body, query) {
			continue
		}
		items = append(items, item)
	}
	return items
}

func mergeEntities(local, remote []Entity, limit int) []Entity {
	byID := make(map[string]Entity, len(local)+len(remote))
	order := make([]string, 0, len(local)+len(remote))
	for _, item := range local {
		key := item.Namespace + ":" + item.ID
		byID[key] = item
		order = append(order, key)
	}
	for _, item := range remote {
		key := item.Namespace + ":" + item.ID
		if previous, exists := byID[key]; exists {
			item.Common = previous.Common
			byID[key] = item
			continue
		}
		byID[key] = item
		order = append(order, key)
	}
	items := make([]Entity, 0, len(order))
	for _, key := range order {
		items = append(items, byID[key])
	}
	return limitEntities(items, limit)
}

func limitEntities(items []Entity, limit int) []Entity {
	if len(items) <= limit {
		return items
	}
	return items[:limit]
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func containsCapability(values []Capability, expected Capability) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func common(id, zhCN, en, entityType string, capabilities ...Capability) Entity {
	if containsCapability(capabilities, CapabilitySpawn) && !containsCapability(capabilities, CapabilityRemove) {
		capabilities = append(capabilities, CapabilityRemove)
	}
	return Entity{
		Key: "dst:" + id, Namespace: "dst", ID: id, NameZhCN: zhCN, NameEn: en,
		Type: entityType, Capabilities: capabilities, Common: true, Source: "builtin",
	}
}

var commonEntities = []Entity{
	common("cutgrass", "草", "Cut Grass", "item", CapabilityGive, CapabilitySpawn),
	common("twigs", "树枝", "Twigs", "item", CapabilityGive, CapabilitySpawn),
	common("log", "木头", "Log", "item", CapabilityGive, CapabilitySpawn),
	common("rocks", "石头", "Rocks", "item", CapabilityGive, CapabilitySpawn),
	common("flint", "燧石", "Flint", "item", CapabilityGive, CapabilitySpawn),
	common("goldnugget", "金块", "Gold Nugget", "item", CapabilityGive, CapabilitySpawn),
	common("charcoal", "木炭", "Charcoal", "item", CapabilityGive, CapabilitySpawn),
	common("boards", "木板", "Boards", "item", CapabilityGive, CapabilitySpawn),
	common("cutstone", "石砖", "Cut Stone", "item", CapabilityGive, CapabilitySpawn),
	common("rope", "绳子", "Rope", "item", CapabilityGive, CapabilitySpawn),
	common("gears", "齿轮", "Gears", "item", CapabilityGive, CapabilitySpawn),
	common("nightmarefuel", "噩梦燃料", "Nightmare Fuel", "item", CapabilityGive, CapabilitySpawn),
	common("livinglog", "活木", "Living Log", "item", CapabilityGive, CapabilitySpawn),
	common("thulecite", "铥矿", "Thulecite", "item", CapabilityGive, CapabilitySpawn),
	common("moonrocknugget", "月岩", "Moon Rock", "item", CapabilityGive, CapabilitySpawn),
	common("silk", "蜘蛛丝", "Silk", "item", CapabilityGive, CapabilitySpawn),
	common("pigskin", "猪皮", "Pig Skin", "item", CapabilityGive, CapabilitySpawn),
	common("nitre", "硝石", "Nitre", "item", CapabilityGive, CapabilitySpawn),
	common("poop", "粪肥", "Manure", "item", CapabilityGive, CapabilitySpawn),
	common("papyrus", "莎草纸", "Papyrus", "item", CapabilityGive, CapabilitySpawn),
	common("marble", "大理石", "Marble", "item", CapabilityGive, CapabilitySpawn),
	common("houndstooth", "犬牙", "Hound's Tooth", "item", CapabilityGive, CapabilitySpawn),
	common("stinger", "针刺", "Stinger", "item", CapabilityGive, CapabilitySpawn),
	common("honey", "蜂蜜", "Honey", "food", CapabilityGive, CapabilitySpawn),
	common("beeswax", "蜂蜡", "Beeswax", "item", CapabilityGive, CapabilitySpawn),
	common("redgem", "红宝石", "Red Gem", "item", CapabilityGive, CapabilitySpawn),
	common("bluegem", "蓝宝石", "Blue Gem", "item", CapabilityGive, CapabilitySpawn),
	common("purplegem", "紫宝石", "Purple Gem", "item", CapabilityGive, CapabilitySpawn),
	common("orangegem", "橙宝石", "Orange Gem", "item", CapabilityGive, CapabilitySpawn),
	common("yellowgem", "黄宝石", "Yellow Gem", "item", CapabilityGive, CapabilitySpawn),
	common("greengem", "绿宝石", "Green Gem", "item", CapabilityGive, CapabilitySpawn),
	common("opalpreciousgem", "彩虹宝石", "Iridescent Gem", "item", CapabilityGive, CapabilitySpawn),
	common("walrus_tusk", "海象牙", "Walrus Tusk", "item", CapabilityGive, CapabilitySpawn),
	common("lightbulb", "荧光果", "Light Bulb", "food", CapabilityGive, CapabilitySpawn),
	common("meatballs", "肉丸", "Meatballs", "food", CapabilityGive, CapabilitySpawn),
	common("dragonpie", "火龙果派", "Dragonpie", "food", CapabilityGive, CapabilitySpawn),
	common("honeyham", "蜜汁火腿", "Honey Ham", "food", CapabilityGive, CapabilitySpawn),
	common("perogies", "波兰水饺", "Pierogi", "food", CapabilityGive, CapabilitySpawn),
	common("baconeggs", "培根煎蛋", "Bacon and Eggs", "food", CapabilityGive, CapabilitySpawn),
	common("bonestew", "炖肉汤", "Meaty Stew", "food", CapabilityGive, CapabilitySpawn),
	common("fishsticks", "炸鱼排", "Fishsticks", "food", CapabilityGive, CapabilitySpawn),
	common("ice", "冰", "Ice", "food", CapabilityGive, CapabilitySpawn),
	common("meat", "肉", "Meat", "food", CapabilityGive, CapabilitySpawn),
	common("monstermeat", "怪物肉", "Monster Meat", "food", CapabilityGive, CapabilitySpawn),
	common("carrot", "胡萝卜", "Carrot", "food", CapabilityGive, CapabilitySpawn),
	common("berries", "浆果", "Berries", "food", CapabilityGive, CapabilitySpawn),
	common("spear", "长矛", "Spear", "item", CapabilityGive, CapabilitySpawn),
	common("hambat", "火腿棒", "Ham Bat", "item", CapabilityGive, CapabilitySpawn),
	common("footballhat", "橄榄球头盔", "Football Helmet", "item", CapabilityGive, CapabilitySpawn),
	common("armorwood", "木甲", "Log Suit", "item", CapabilityGive, CapabilitySpawn),
	common("backpack", "背包", "Backpack", "item", CapabilityGive, CapabilitySpawn),
	common("torch", "火炬", "Torch", "item", CapabilityGive, CapabilitySpawn),
	common("axe", "斧头", "Axe", "item", CapabilityGive, CapabilitySpawn),
	common("pickaxe", "鹤嘴锄", "Pickaxe", "item", CapabilityGive, CapabilitySpawn),
	common("shovel", "铲子", "Shovel", "item", CapabilityGive, CapabilitySpawn),
	common("hammer", "锤子", "Hammer", "item", CapabilityGive, CapabilitySpawn),
	common("bugnet", "捕虫网", "Bug Net", "item", CapabilityGive, CapabilitySpawn),
	common("fishingrod", "淡水钓竿", "Freshwater Fishing Rod", "item", CapabilityGive, CapabilitySpawn),
	common("minerhat", "矿工帽", "Miner Hat", "item", CapabilityGive, CapabilitySpawn),
	common("lantern", "提灯", "Lantern", "item", CapabilityGive, CapabilitySpawn),
	common("cane", "步行手杖", "Walking Cane", "item", CapabilityGive, CapabilitySpawn),
	common("tentaclespike", "触手尖刺", "Tentacle Spike", "item", CapabilityGive, CapabilitySpawn),
	common("nightsword", "暗夜剑", "Dark Sword", "item", CapabilityGive, CapabilitySpawn),
	common("ruins_bat", "铥矿棒", "Thulecite Club", "item", CapabilityGive, CapabilitySpawn),
	common("armorgrass", "草甲", "Grass Suit", "item", CapabilityGive, CapabilitySpawn),
	common("armormarble", "大理石甲", "Marble Suit", "item", CapabilityGive, CapabilitySpawn),
	common("armorruins", "铥矿甲", "Thulecite Suit", "item", CapabilityGive, CapabilitySpawn),
	common("eyebrellahat", "眼球伞", "Eyebrella", "item", CapabilityGive, CapabilitySpawn),
	common("panflute", "排箫", "Pan Flute", "item", CapabilityGive, CapabilitySpawn),
	common("amulet", "重生护符", "Life Giving Amulet", "item", CapabilityGive, CapabilitySpawn),
	common("reviver", "告密的心", "Telltale Heart", "item", CapabilityGive, CapabilitySpawn),
	common("healingsalve", "治疗药膏", "Healing Salve", "item", CapabilityGive, CapabilitySpawn),
	common("bandage", "蜂蜜药膏", "Honey Poultice", "item", CapabilityGive, CapabilitySpawn),
	common("beefalo", "皮弗娄牛", "Beefalo", "creature", CapabilitySpawn),
	common("pigman", "猪人", "Pig Man", "creature", CapabilitySpawn),
	common("bunnyman", "兔人", "Bunnyman", "creature", CapabilitySpawn),
	common("spider", "蜘蛛", "Spider", "creature", CapabilitySpawn),
	common("hound", "猎犬", "Hound", "creature", CapabilitySpawn),
	common("tallbird", "高脚鸟", "Tallbird", "creature", CapabilitySpawn),
	common("lightninggoat", "伏特羊", "Volt Goat", "creature", CapabilitySpawn),
	common("catcoon", "浣猫", "Catcoon", "creature", CapabilitySpawn),
	common("rabbit", "兔子", "Rabbit", "creature", CapabilitySpawn),
	common("mole", "鼹鼠", "Moleworm", "creature", CapabilitySpawn),
	common("walrus", "海象", "MacTusk", "creature", CapabilitySpawn),
	common("merm", "鱼人", "Merm", "creature", CapabilitySpawn),
	common("tentacle", "触手", "Tentacle", "creature", CapabilitySpawn),
	common("worm", "洞穴蠕虫", "Depths Worm", "creature", CapabilitySpawn),
	common("krampus", "坎普斯", "Krampus", "creature", CapabilitySpawn),
	common("warg", "座狼", "Varg", "creature", CapabilitySpawn),
	common("deerclops", "独眼巨鹿", "Deerclops", "boss", CapabilitySpawn),
	common("bearger", "熊獾", "Bearger", "boss", CapabilitySpawn),
	common("moose", "麋鹿鹅", "Moose/Goose", "boss", CapabilitySpawn),
	common("dragonfly", "龙蝇", "Dragonfly", "boss", CapabilitySpawn),
	common("spiderqueen", "蜘蛛女王", "Spider Queen", "boss", CapabilitySpawn),
	common("antlion", "蚁狮", "Antlion", "boss", CapabilitySpawn),
	common("beequeen", "蜂王", "Bee Queen", "boss", CapabilitySpawn),
	common("klaus", "克劳斯", "Klaus", "boss", CapabilitySpawn),
	common("toadstool", "毒菌蟾蜍", "Toadstool", "boss", CapabilitySpawn),
	common("minotaur", "远古守护者", "Ancient Guardian", "boss", CapabilitySpawn),
	common("stalker_atrium", "远古织影者", "Ancient Fuelweaver", "boss", CapabilitySpawn),
	common("alterguardian_phase3", "天体英雄", "Celestial Champion", "boss", CapabilitySpawn),
	common("crabking", "帝王蟹", "Crab King", "boss", CapabilitySpawn),
	common("malbatross", "邪天翁", "Malbatross", "boss", CapabilitySpawn),
	common("eyeofterror", "恐怖之眼", "Eye of Terror", "boss", CapabilitySpawn),
	common("twinofterror1", "激光眼", "Retinazor", "boss", CapabilitySpawn),
	common("twinofterror2", "魔焰眼", "Spazmatism", "boss", CapabilitySpawn),
	common("daywalker", "梦魇疯猪", "Nightmare Werepig", "boss", CapabilitySpawn),
	common("campfire", "营火", "Campfire", "structure", CapabilitySpawn),
	common("firepit", "火坑", "Fire Pit", "structure", CapabilitySpawn),
	common("researchlab", "科学机器", "Science Machine", "structure", CapabilitySpawn),
	common("researchlab2", "炼金引擎", "Alchemy Engine", "structure", CapabilitySpawn),
	common("researchlab3", "暗影操控器", "Shadow Manipulator", "structure", CapabilitySpawn),
	common("researchlab4", "灵子分解器", "Prestihatitator", "structure", CapabilitySpawn),
	common("icebox", "冰箱", "Ice Box", "structure", CapabilitySpawn),
	common("treasurechest", "箱子", "Chest", "structure", CapabilitySpawn),
	common("cookpot", "烹饪锅", "Crock Pot", "structure", CapabilitySpawn),
	common("birdcage", "鸟笼", "Birdcage", "structure", CapabilitySpawn),
	common("lightning_rod", "避雷针", "Lightning Rod", "structure", CapabilitySpawn),
	common("pighouse", "猪屋", "Pig House", "structure", CapabilitySpawn),
	common("rabbithouse", "兔屋", "Rabbit Hutch", "structure", CapabilitySpawn),
}
