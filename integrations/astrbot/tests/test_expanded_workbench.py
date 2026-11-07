"""Fixed player modes use the dynamic directory and its confirmation policy."""
import sys
import unittest
from pathlib import Path
from dataclasses import replace
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from test_companion import actor
from test_workbench import recipe
from astrbot_plugin_dst_companion.interactions import direct_request, interaction
from astrbot_plugin_dst_companion.capabilities import permitted

class ExpandedWorkbenchTests(unittest.TestCase):
 def item(self):
  item=recipe('workbench.set_player_mode','high')
  item['parameters']['properties'].update(mode={'type':'string','enum':['invincible','super_god','creative','health_lock','one_hit','invisible','water_walk','noclip','clear_all']},enabled={'type':'boolean'})
  item['parameters']['required']+=['mode','enabled']
  item['interaction']={'kind':'preview-confirm-v1','resultVersion':2,'effectVersion':'1'}
  item['behaviorRevision']='6'
  return item
 def test_each_mode_routes_without_model_and_leaves_confirmation_to_the_directory(self):
  user=replace(actor(role='game-manage'),workbench_mode='all');item=self.item()
  for name,mode in [('普通无敌','invincible'),('超级无敌','super_god'),('制造','creative'),('锁血','health_lock'),('一击必杀','one_hit'),('隐身','invisible'),('踏水','water_walk'),('穿墙','noclip')]:
   for word,enabled in [('开启',True),('关闭',False)]:
    with self.subTest(name=name,enabled=enabled):
     result=direct_request(f'给 lcy {word}{name}模式',[item],user)
     self.assertIsNotNone(result);self.assertEqual(result[1],{'player_id':'lcy','mode':mode,'enabled':enabled})
     self.assertEqual(interaction(item,result[1])['resultVersion'],2)
     self.assertNotIn('confirmed',result[1]);self.assertNotIn('phase',result[1])
 def test_denial_discussion_and_read_role_never_generate_write_request(self):
  item=self.item();manager=replace(actor(role='game-manage'),workbench_mode='all')
  for text in ['别给lcy开启穿墙','给lcy开启穿墙会怎样？','如果lcy开启穿墙','给lcy开启穿墙然后停止世界']:
   self.assertIsNone(direct_request(text,[item],manager))
  self.assertFalse(permitted(actor(),item))
  self.assertIsNone(direct_request('给lcy开启穿墙',[item],actor()))
  self.assertIsNone(direct_request('给lcy开启穿墙',[],manager))
 def test_new_panel_same_protocol_uses_existing_plugin_and_unknown_protocol_refuses(self):
  item=self.item();self.assertEqual(interaction(item,{})['kind'],'preview-confirm-v1')
  item['interaction']['resultVersion']=99
  from astrbot_plugin_dst_companion.gateway import QueryError
  with self.assertRaises(QueryError):interaction(item,{})
