# 工作台支持范围

工作台、MCP 和官方 AstrBot 插件共用服务端固定操作。页面从“游戏工作台”进入；
新增功能集中在“更多工具”，物品卡片右键或右上角菜单提供常用操作。
机器人通过 `dst_find` 按中文查找，再使用原有任务流程准备和确认。

| 功能 | 页面／固定能力 |
| --- | --- |
| 资源分类、拼音与首字母搜索、收藏 | 资源目录；收藏、制作、地皮、模组等分类，支持触屏菜单 |
| 批量生成、皮肤记忆 | 资源菜单／`workbench.spawn_near_player`；皮肤由游戏核对归属 |
| 地图选点生成、网格对齐 | 地皮与地图／`workbench.spawn_entity_at`；每次选点对应一批确认 |
| 获取材料、蓝图、解锁单个配方 | `give_recipe_materials`、`give_blueprint`、`unlock_player_recipe` |
| 同类数量、数量公告、轮流定位、全图同类清理 | `count_world_entities`、`announce_entity_count`、`cycle_entity_teleport`、`remove_world_entities` |
| 保存地点、同世界传送 | 地点页；`location.save`、`player.teleport`，支持删除已保存地点 |
| 地图中心、跨世界传送 | `teleport_world_center`、`migrate_player`；跨世界使用游戏原生迁移流程 |
| 生命、饥饿、理智、湿度、体温、上限惩罚 | 玩家操作；`set_player_stat`、`set_player_health_value`、`set_health_penalty`、`match_world_temperature` |
| 状态恢复、基础工具、饥饿速率、移速 | `restore_player_stats`、`give_starter_kit`、`set_player_hunger_rate`、`set_player_speed` |
| 清空携带物品、死亡、复活、重选角色、芜猴变化 | 玩家操作及原有玩家 API／固定工作台命令 |
| 八种模式与一键解除 | `set_player_mode`：普通无敌、超级无敌、制造、锁血、必杀、隐身、踏水、穿墙 |
| 模式记忆 | `set_mode_memory`；默认关闭，明确开启后恢复本世界为该玩家记住的面板模式 |
| 全员召集、死亡、复活、状态恢复 | `relocate_players`、`apply_all_players`；作用于所选世界 |
| 四季、昼夜、指定天数、时间倍率、月相、天气 | 原有世界操作及 `set_world_day`、`set_daylight_mode` |
| 闪电、月雹、流星雨、月球风暴、远古阶段、重置远古 | `trigger_world_event`；自定义远古周期用 `set_nightmare_segments` |
| 幸运值、淘气值、隐士装饰评分公告 | `announce_game_value`；组件或目标不存在时明确拒绝 |
| 附近删除、击杀、灭火、修复烧毁、冻结、催眠、平息仇恨 | `act_nearby_entities`；重建烧毁建筑限已适配类型 |
| 施肥、生长、催熟、采摘、拾取、巨大农作物、农田养分水分 | `act_nearby_entities`、`ripen_giant_crops`、`fertilize_farm_soil` |
| 附近生物恢复、引导移动、允许／禁止移动 | `act_nearby_entities`、`move_nearby_creatures`、`set_nearby_movement` |
| 角色资源、伙伴与用品 | 角色专项页：18 个原版角色用品，及胡须、阿比盖尔、沃比、电能、灵感、健身值、兽形、开花、旺达年龄、沃特形态等实际组件能力 |
| 洞察点、重置技能、技能同步、地图探索 | `manage_player_skills`、`repair_skill_sync`、`reveal_player_map` |
| 攻速人偶、互通虫洞、梦魇疯猪、天体材料、拾荒疯猪 | `spawn_special_structure`；检查世界、落点与原生组件前提 |
| 岩石树两种阵列、蜂箱与灭火器、池塘圆环 | `spawn_entity_array` |
| 发放地皮、铺设地皮 | `give_turf`、`edit_world_tiles`；特殊地面未必有可发放的地皮物品 |
| 保存、回档、重载、重置世界、移除道路 | 原有世界操作及 `remove_world_roads`；道路移除在保存回调后重载 |
| 游戏管理员名单 | 沿名单页及现有名单 API，继续要求面板管理权限 |
| 控制台与调试 | 原有管理员控制台、日志和采样诊断；不提供第三方模组的收费授权功能 |
| 操作偏好 | 使用设置：数量、范围、中键六种快捷方式、提示六种方式、代码、网格、收藏、模组来源等 |

参考模组的“日食”实际切换全天白昼／黑夜，本项目按实际效果命名。
角色用品不等于每个角色都有独立数值设置；只操作当前版本确实存在的组件。
客户端错误技能弹窗需要本机处理，可使用[可选客户端助手](../integrations/dst-admin-client/README.md)，
不能把服务端同步当成已经修好另一台电脑的界面。

所有修改继续要求管理权限。普通读取不能公告、保存或修改世界；风险低也不等于只读。
管理员名单不通过游戏脚本改文件。固定命令不接受任意 Lua 或调用者指定的函数。

扩展功能需要 Runtime 2.4.24；旧 Runtime 明确提示更新，不回退到通用脚本。
Runtime 2.4.26 起，玩家操作、工作台与旧能力入口共用模式开关和模式记忆。
例如，玩家操作开启的制造模式可以在工作台关闭，“解除全部模式”也能解除这些入口
开启的八种模式。普通无敌只免伤；超级无敌／上帝模式还会持续恢复状态，二者保持独立。
踏水、穿墙与上帝模式同时开启时，关闭其中一种会保留其余模式需要的效果；
潜行与免伤、不同来源的锁血也只解除本次操作所持有的效果。
“解除全部模式”仅针对表中的八种模式；独立的潜行、不吸引仇恨和指定数值锁定仍分别关闭。
一击必杀沿用玩家目标的正常伤害，并尊重目标的格挡保护。
模组替换了伤害函数、碰撞或其他受管状态时，会提示效果无法确认，不强行覆盖模组。
旧版已开启的一击必杀、潜行或锁血若无法确认恢复依据，会提示该玩家重新进入游戏后再操作，
无需因此重启世界。

旧 API 与原有回执继续保留。新面板配旧插件可以继续使用其已识别的固定交互，
插件未支持的新交互会提示更新，不自动下载安装代码，也不自动扩大群权限。

具体参数、确认与资源限制见[工作台操作说明](workbench-actions.md)、
[保存地点与玩家传送](locations-and-teleport.md)。
