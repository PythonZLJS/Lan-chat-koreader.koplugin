"""Lua 5.1/LuaJIT contract tests with KOReader widget stubs, not a device emulator."""
from pathlib import Path
import json
import os
import sys
import tempfile
import unittest

ROOT=Path(__file__).resolve().parents[2]
for candidate in [ROOT/'koreader/build/testdeps', ROOT.parent/'KindleRemote/koreader/build/testdeps',ROOT.parent/'dayline-koreader/build/testdeps']:
    if candidate.exists(): sys.path.insert(0,str(candidate))
from lupa.luajit21 import LuaRuntime

STUBS=r'''
local Base={}
function Base:extend(o) o=o or {};setmetatable(o,{__index=self});o.__index=o;return o end
function Base:new(o)
 o=o or {};setmetatable(o,{__index=self});o.key_events={};if o.init then o:init() end;return o
end
function Base:getSize() return self.dimen or {w=self.width or 0,h=self.height or 0} end
function Base:handleEvent(e) local f=self[e.handler];if f then return f(self,unpack(e.args or {})) end end
function Base:onShowKeyboard() keyboard_count=keyboard_count+1 end
function Base:getInputText() return self.input end
function Base:getFields() local r={};for i,f in ipairs(self.fields) do r[i]=f.text end;return r end
local function preload(name,value) package.preload[name]=function()return value end end
screenW,screenH,screenScale=1072,1448,1.6
local Screen={getWidth=function()return screenW end,getHeight=function()return screenH end,scaleBySize=function(_,n)return math.floor(n*screenScale) end}
preload('device',{screen=Screen,isKindle=function()return true end,hasKeys=function()return true end,input={group={Back='Back'}}})
preload('ui/widget/container/widgetcontainer',Base)
preload('ui/widget/container/inputcontainer',Base)
local Frame=Base:extend{}
function Frame:getSize()local s=self[1]:getSize();return{w=s.w+2*((self.padding or 0)+(self.bordersize or 0)),h=s.h+2*((self.padding or 0)+(self.bordersize or 0))}end
preload('ui/widget/container/framecontainer',Frame)
local Group=Base:extend{}
function Group:getSize()local w,h=0,0;for _,v in ipairs(self)do local s=v:getSize();w=math.max(w,s.w);h=h+s.h end;return{w=w,h=h}end
preload('ui/widget/verticalgroup',Group)
local Span=Base:extend{}
function Span:getSize()return{w=0,h=self.width or 0}end
preload('ui/widget/verticalspan',Span)
local Title=Base:extend{}
function Title:getSize()return{w=self.width,h=Screen:scaleBySize(40)}end
preload('ui/widget/titlebar',Title)
local Buttons=Base:extend{}
function Buttons:getSize()return{w=self.width,h=#self.buttons*Screen:scaleBySize(48)}end
preload('ui/widget/buttontable',Buttons)
preload('ui/widget/scrolltextwidget',Base)
preload('ui/widget/textboxwidget',Base)
for _,name in ipairs({'buttondialog','inputdialog','multiinputdialog','confirmbox','infomessage','textviewer','menu','pathchooser'})do preload('ui/widget/'..name,Base)end
preload('ui/font',{getFace=function(_,name,size)return{name=name,size=size}end})
preload('ui/geometry',{new=function(_,o)return o end})
preload('ffi/blitbuffer',{COLOR_WHITE=255})
timers={};shown={};keyboard_count=0;page_turns=0;actions={}
manager={scheduleIn=function(_,delay,fn)timers[fn]=delay end,unschedule=function(_,fn)timers[fn]=nil end,
 nextTick=function(_,fn)fn()end,setDirty=function()end,
 show=function(_,w,mode,region,x,y)shown[#shown+1]=w;if w.dimen then w.dimen.x=x or 0;w.dimen.y=y or 0 end end,
 close=function(_,w)if w.onCloseWidget then w:onCloseWidget()end end}
preload('ui/uimanager',manager)
preload('ui/network/manager',{runWhenConnected=function(_,fn)fn()end})
preload('dispatcher',{registerAction=function(_,key,value)actions[key]=value end})
preload('json',{encode=py_encode,decode=py_decode})
function test_ui()return{document={},menu={registerToMainMenu=function()end,onTapCloseMenu=function()end},handleEvent=function()page_turns=page_turns+1;return true end}end
'''

class PluginTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.lua=LuaRuntime(unpack_returned_tuples=True)
        self.lua.globals().py_encode=lambda value:json.dumps(self.plain(value),ensure_ascii=False)
        self.lua.globals().py_decode=lambda text:self.table(json.loads(text))
        self.lua.execute(STUBS)
        self.path=ROOT/'koreader/neighbourchat.koplugin'
        self.client_class=self.lua.execute('return dofile(...)',(self.path/'client.lua').as_posix())
        self.requests=[]
        self.state=dict(version=1,stage='connected',connected=True,admin=True,hosting=True,address='This Kindle',display='Alex',busy=False,
                        messages=[{'display_name':'Jamie','body':'Keep reading; chat remains connected.'}],users=[],more=False)
        self.token_file=Path(self.temp.name)/'token.txt';self.token_file.write_text('k'*43)
        self.client=self.client_class.new(self.path.as_posix(),self.lua.table_from({'transport':self.transport,'token_file':str(self.token_file)}))
        self.lua.globals().test_client=self.client
        self.lua.execute('package.loaded["neighbourchat.session"]={client=test_client,ready=true,draft="",full=false}')
        self.cls=self.lua.execute('return dofile(...)',(self.path/'main.lua').as_posix())
        self.plugin=self.cls.new(self.cls,self.lua.table_from({'ui':self.lua.eval('test_ui()')}))

    def table(self,value):
        if isinstance(value,dict):return self.lua.table_from({k:self.table(v)for k,v in value.items()})
        if isinstance(value,list):return self.lua.table_from([self.table(v)for v in value])
        return value
    def plain(self,value):
        if hasattr(value,'items'):
            pairs=dict(value.items())
            if pairs and set(pairs)==set(range(1,len(pairs)+1)):return [self.plain(pairs[i])for i in range(1,len(pairs)+1)]
            return{k:self.plain(v)for k,v in pairs.items()}
        return value
    def transport(self,method,path,body,token,etag=None):
        self.requests.append((method,path,body,token))
        if path=='/_health':return 200,json.dumps({'service':'neighbour-chat-v3','reader_api':4})
        if token!='k'*43:return 403,'{"error":"not authorised"}'
        if path=='/_reader/state':return 200,json.dumps(self.state)
        data=json.loads(body)
        if data['action']=='disconnect':self.state.update(stage='idle',connected=False,messages=[])
        if data['action']=='send':self.state.update(sent_token=data['token'],notice='Message sent.')
        return 202,'{"accepted":true}'
    def call(self,name,*args):return self.plugin[name](self.plugin,*args)
    def test_compile_all_lua_with_luajit(self):
        for path in self.path.glob('*.lua'):self.lua.execute('assert(loadfile(...))',path.as_posix())
    def test_hide_and_book_change_keep_session_without_network_actions(self):
        self.call('open',False);self.call('tick')
        self.assertTrue(self.client.state.connected)
        self.call('hide')
        self.assertFalse([r for r in self.requests if r[0]=='POST'])
        self.assertEqual(0,self.lua.eval('(function()local n=0;for _ in pairs(timers)do n=n+1 end;return n end)()'))
        other=self.cls.new(self.cls,self.lua.table_from({'ui':self.lua.eval('test_ui()')}))
        other.open(other,True);other.tick(other)
        self.assertTrue(self.client.state.connected)
        self.assertIn('Keep reading',other.view.text)
    def test_both_layouts_fit_portrait_and_landscape(self):
        for width,height,scale in [(600,800,1),(1072,1448,1.6),(1264,1680,1.8),(800,600,1)]:
            self.lua.globals().screenW=width;self.lua.globals().screenH=height;self.lua.globals().screenScale=scale
            for full in [False,True]:
                self.call('open',full);self.call('tick');v=self.plugin.view
                self.assertLessEqual(v.dimen.x+v.dimen.w,width)
                self.assertLessEqual(v.dimen.y+v.dimen.h,height)
                self.assertEqual(full,bool(v.covers_fullscreen))
                if full:self.assertEqual(height,v.dimen.h)
                else:self.assertLess(v.dimen.h,height*.65)
    def test_popup_passes_outside_taps_to_book_but_inside_is_consumed(self):
        self.call('open',False);self.call('tick')
        event=self.table({'handler':'onGesture','args':[{'ges':'tap','pos':{'x':1,'y':1}}]})
        self.plugin.view.handleEvent(self.plugin.view,event)
        self.assertEqual(1,self.lua.globals().page_turns)
        inside=self.table({'handler':'onGesture','args':[{'ges':'tap','pos':{'x':self.plugin.view.dimen.x+10,'y':self.plugin.view.dimen.y+10}}]})
        self.plugin.view.handleEvent(self.plugin.view,inside)
        self.assertEqual(1,self.lua.globals().page_turns)
        self.call('open',True);self.plugin.view.handleEvent(self.plugin.view,event)
        self.assertEqual(1,self.lua.globals().page_turns)
    def test_stop_is_distinct_from_hiding(self):
        self.call('open',False);self.call('tick');self.call('stop')
        confirm=self.lua.globals().shown[len(self.lua.globals().shown)]
        confirm.ok_callback()
        self.assertTrue(any(r[0]=='POST'and json.loads(r[2])['action']=='stop'for r in self.requests))
        self.assertIsNone(self.plugin.view)
        self.assertFalse(self.lua.eval('package.loaded["neighbourchat.session"].ready'))
    def test_disconnect_clears_drafts_without_stopping_host(self):
        self.call('open',False);self.call('tick')
        self.lua.execute('package.loaded["neighbourchat.session"].draft="private unsent"')
        self.call('action',self.table({'action':'disconnect'}));self.call('tick')
        self.assertFalse(self.client.state.connected)
        self.assertTrue(self.client.state.hosting)
        self.assertEqual('',self.lua.eval('package.loaded["neighbourchat.session"].draft'))
    def test_no_keyboard_on_open_and_gesture_actions_are_registered(self):
        self.call('open',False);self.call('tick');self.call('open',True)
        self.assertEqual(0,self.lua.globals().keyboard_count)
        self.assertIsNotNone(self.lua.globals().actions.neighbourchat_popup)
        self.assertIsNotNone(self.lua.globals().actions.neighbourchat_full)
    def test_unknown_local_paths_and_older_service_are_rejected(self):
        value=self.client.call(self.client,'GET','http://evil.invalid',None)
        self.assertIsNone(value[0])
        launched=[]
        c=self.client_class.new(self.path.as_posix(),self.lua.table_from({'transport':lambda *args:(200,'{"service":"neighbour-chat-v3"}'),'launch':lambda:launched.append(True)}))
        value=c.start(c)
        self.assertIsNone(value[0]);self.assertFalse(launched)
    def test_confirmed_send_does_not_erase_a_later_draft(self):
        self.call('open',False);self.call('tick')
        self.lua.execute('package.loaded["neighbourchat.session"].send_token="confirmed-key";package.loaded["neighbourchat.session"].draft="sent"')
        self.state.update(sent_token='confirmed-key',notice='Message sent.')
        self.call('tick')
        self.assertEqual('',self.lua.eval('package.loaded["neighbourchat.session"].draft'))
        self.lua.execute('package.loaded["neighbourchat.session"].draft="new unsent text"')
        self.call('tick')
        self.assertEqual('new unsent text',self.lua.eval('package.loaded["neighbourchat.session"].draft'))
    def test_send_acknowledgement_keeps_a_newer_unsent_draft(self):
        self.call('open',False);self.call('tick')
        self.lua.execute('local s=package.loaded["neighbourchat.session"];s.send_token="pending";s.sent_body="sent text";s.draft="newer unsent text"')
        self.state.update(sent_token='pending')
        self.call('tick')
        self.assertEqual('newer unsent text',self.lua.eval('package.loaded["neighbourchat.session"].draft'))

    def test_suspend_resume_do_not_disconnect(self):
        self.call('onSuspend');self.call('onResume')
        self.assertEqual(['pause','resume'],[json.loads(r[2])['action']for r in self.requests if r[0]=='POST'])
    def test_editor_back_preserves_draft_and_explicit_send_uses_retry_key(self):
        self.call('open',False);self.call('tick');self.call('write')
        editor=self.lua.globals().shown[len(self.lua.globals().shown)]
        self.assertTrue(editor.allow_newline)
        editor.input='Draft kept when pressing Back'
        self.lua.globals().manager.close(self.lua.globals().manager,editor)
        self.assertIsNone(self.plugin.dialog)
        self.assertEqual(editor.input,self.lua.eval('package.loaded["neighbourchat.session"].draft'))
        self.call('write')
        editor=self.lua.globals().shown[len(self.lua.globals().shown)]
        editor.buttons[1][2].callback()
        sent=[json.loads(r[2])for r in self.requests if r[0]=='POST'and json.loads(r[2])['action']=='send']
        self.assertEqual(1,len(sent));self.assertGreaterEqual(len(sent[0]['token']),16)
        self.call('tick')
        self.assertEqual('',self.lua.eval('package.loaded["neighbourchat.session"].draft'))
    def test_cached_poll_does_not_redraw_unchanged_screen(self):
        self.call('open',False);self.call('tick')
        count=len(self.lua.globals().shown)
        self.call('tick');self.call('tick')
        self.assertEqual(count,len(self.lua.globals().shown))
    def test_unchanged_local_state_uses_conditional_request(self):
        seen=[]
        def cached(method,path,body,token,etag=None):
            seen.append(etag)
            if etag=='"4-true"':return 304,'','"4-true"'
            return 200,json.dumps(self.state),'"4-true"'
        self.client.request=cached
        self.client.readState(self.client);self.client.readState(self.client)
        self.assertEqual([None,'"4-true"'],seen)
        self.assertTrue(self.client.state.connected)
    def test_navigation_tabs_are_visible_and_route_without_disconnect(self):
        self.call('open',True);self.call('tick')
        tabs=self.plugin.view.navigation.buttons[1]
        self.assertEqual(4,len(tabs))
        tabs[1].callback()
        self.assertEqual('connect',self.plugin.view.model.screen)
        tabs=self.plugin.view.navigation.buttons[1];tabs[3].callback()
        self.assertEqual('people',self.plugin.view.model.screen)
        self.assertFalse(any(json.loads(r[2])['action']=='disconnect'for r in self.requests if r[0]=='POST'))

    def test_recent_hosts_have_direct_reconnect_and_forget_controls(self):
        self.state.update(stage='idle',connected=False,recent_hosts=['https://192.168.1.20:8000'])
        self.call('open',True);self.call('tick')
        self.call('navigate','recent')
        rows=self.plugin.view.model.rows
        self.assertEqual('reconnect',rows[1].id)
        self.assertEqual('forget',rows[1].secondary.id)
        self.plugin.view.actions[1].buttons[1][1].callback()
        posted=[json.loads(r[2])for r in self.requests if r[0]=='POST']
        self.assertEqual('reconnect',posted[-1]['action'])
        self.assertEqual('https://192.168.1.20:8000',posted[-1]['address'])

    def test_every_screen_fits_with_long_lists_and_error_text(self):
        self.state.update(users=[dict(id=i,display_name='Reader '+str(i),username='reader'+str(i))for i in range(100)],
            rooms=['Lobby']+['Room '+str(i)for i in range(50)],recent_hosts=['https://192.168.1.'+str(i)+':8000'for i in range(1,9)],
            error='An actionable error message that needs two lines on a small display.')
        for width,height,scale in [(600,800,1),(800,600,1),(1072,1448,1.6),(1264,1680,1.8)]:
            self.lua.globals().screenW=width;self.lua.globals().screenH=height;self.lua.globals().screenScale=scale
            self.call('open',True);self.call('tick')
            for screen in ['connect','chat','people','rooms','host','inbox','search','recent','nearby']:
                self.call('navigate',screen)
                v=self.plugin.view
                self.assertLessEqual(v.dimen.w,width,(screen,width,height))
                self.assertLessEqual(v.dimen.h,height,(screen,width,height))
                if screen=='people':self.assertIsNotNone(v.pager)

    def test_popup_has_only_write_expand_hide_controls(self):
        self.call('open',False);self.call('tick')
        self.assertEqual(0,len(self.plugin.view.model.tabs))
        tools=self.plugin.view.model.tools[1]
        self.assertEqual(['write','full','hide'],[tools[i].id for i in range(1,4)])

    def test_home_offers_host_and_find_and_scan_runs_outside_input_dialog(self):
        self.state.update(stage='idle',connected=False,hosting=False,admin=False)
        self.call('open',True);self.call('tick')
        self.assertEqual(['host','scan','navigate'],[self.plugin.view.model.rows[i].id for i in range(1,4)])
        self.call('dispatch','scan',None)
        self.assertEqual('nearby',self.plugin.view.model.screen)
        self.assertEqual(0,self.lua.globals().keyboard_count)
        self.assertEqual('scan',json.loads(self.requests[-1][2])['action'])

    def test_nearby_chat_is_joined_without_typing_ip(self):
        self.state.update(stage='idle',connected=False,nearby=[dict(name='Reading group',address='https://192.168.1.40:8000')],scanned=True)
        self.call('open',True);self.call('tick');self.call('navigate','nearby')
        entry=self.plugin.view.model.rows[1]
        self.assertEqual('Reading group',entry.text)
        self.call('dispatch',entry.id,entry.value)
        data=json.loads(self.requests[-1][2])
        self.assertEqual(dict(action='join-nearby',address='https://192.168.1.40:8000'),data)
        self.assertEqual(0,self.lua.globals().keyboard_count)

    def test_discovery_switch_is_visible_and_does_not_disconnect(self):
        self.state.update(discoverable=True,discovery_name='Reading group')
        self.call('open',True);self.call('tick');self.call('navigate','host')
        self.assertIn('Discovery ON',self.plugin.view.model.description)
        self.assertEqual('discovery-off',self.plugin.view.model.rows[1].id)
        self.call('dispatch','discovery-off',None)
        posted=[json.loads(r[2])['action']for r in self.requests if r[0]=='POST']
        self.assertIn('discovery-off',posted)
        self.assertNotIn('disconnect',posted)

    def test_host_change_clears_private_draft_recipient_and_retry_key(self):
        self.state.update(context='host-one',user_id=2,room='Lobby')
        self.call('open',True);self.call('tick')
        self.lua.execute('local s=package.loaded["neighbourchat.session"];s.draft="secret";s.recipient={id="3",label="Alice"};s.send_token="retry"')
        self.state.update(context='host-two',address='https://192.168.1.88:8000')
        self.call('tick')
        self.assertEqual('',self.lua.eval('package.loaded["neighbourchat.session"].draft'))
        self.assertIsNone(self.lua.eval('package.loaded["neighbourchat.session"].recipient'))
        self.assertIsNone(self.lua.eval('package.loaded["neighbourchat.session"].send_token'))

    def test_open_editor_cannot_send_after_chat_or_room_changes(self):
        self.state.update(context='same-host',room='Lobby')
        self.call('open',True);self.call('tick');self.call('write')
        editor=self.lua.globals().shown[len(self.lua.globals().shown)];editor.input='Private draft for old room'
        self.state.update(context='new-room',room='Books');self.call('tick')
        editor.buttons[1][2].callback()
        self.assertFalse(any(json.loads(r[2])['action']=='send'for r in self.requests if r[0]=='POST'))
        self.assertEqual('',self.lua.eval('package.loaded["neighbourchat.session"].draft'))

    def test_old_login_form_cannot_send_password_to_new_host(self):
        self.state.update(stage='login',connected=False,context='first',address='https://192.168.1.20:8000')
        self.call('open',True);self.call('tick');self.call('login')
        form=self.lua.globals().shown[len(self.lua.globals().shown)]
        form.fields[1].text='reader';form.fields[2].text='secret password'
        self.state.update(context='second',address='https://192.168.1.30:8000');self.call('tick')
        form.buttons[1][2].callback()
        self.assertFalse(any(json.loads(r[2])['action']=='login'for r in self.requests if r[0]=='POST'))

    def test_rejected_action_does_not_run_followup_on_unrelated_state(self):
        self.call('open',True);self.call('tick')
        original=self.transport
        self.client.request=lambda method,*args:(409,'{"error":"Busy"}')if method=='POST'else original(method,*args)
        called=[]
        self.call('showAfter',self.table(dict(action='files')),lambda state:called.append(True))
        self.assertFalse(called)
        self.assertEqual(1,self.lua.eval('(function()local n=0;for _ in pairs(timers)do n=n+1 end;return n end)()'))

    def raw_client(self):
        self.lua.execute(r'''
        package.preload['socket']=function()return{gettime=function()return 0 end,tcp=function()
         local data=raw_response;local pos=1
         return{settimeout=function()end,connect=function(_,host,port)assert(host=='127.0.0.1'and port==8001);return true end,
          send=function(_,request)raw_sent=request;return #request end,close=function()end,
          receive=function(_,mode)
           if mode=='*l'then local ending=data:find('\n',pos,true);if not ending then return nil,'closed',''end
            local line=data:sub(pos,ending-1):gsub('\r$','');pos=ending+1;return line end
           local chunk=data:sub(pos,pos+mode-1);pos=pos+#chunk
           if #chunk<mode then return nil,'closed',chunk end;return chunk
          end}
        end}end
        package.loaded['socket']=nil
        ''')
        return self.client_class.new(self.path.as_posix(),self.lua.table_from({'token_file':str(self.token_file)}))
    def test_raw_loopback_protocol_parses_utf8_and_etags(self):
        client=self.raw_client()
        state=dict(self.state,messages=[{'display_name':'Renée','body':'Hello £ world\n'*600}])
        body=json.dumps(state,ensure_ascii=False).encode('utf-8')
        self.lua.globals().raw_response=b'HTTP/1.0 200 OK\r\nContent-Length: '+str(len(body)).encode()+b'\r\nEtag: "7-true"\r\n\r\n'+body
        value=client.readState(client)
        self.assertIsNotNone(value)
        self.assertEqual('"7-true"',client.etag)
        self.assertEqual('Renée',client.state.messages[1].display_name)
        self.assertIn('Authorization: Bearer '+'k'*43,self.lua.globals().raw_sent)
        self.lua.globals().raw_response=b'HTTP/1.0 304 Not Modified\r\nEtag: "7-true"\r\n\r\n'
        client.readState(client)
        self.assertIn('If-None-Match: "7-true"',self.lua.globals().raw_sent)
        self.assertTrue(client.state.connected)
    def test_raw_loopback_protocol_rejects_oversized_response(self):
        client=self.raw_client()
        self.lua.globals().raw_response=b'HTTP/1.0 200 OK\r\nContent-Length: 99999999\r\n\r\n'
        result=client.readState(client)
        self.assertIsNone(result[0])
        self.assertIn('large',result[1])

if __name__=='__main__':unittest.main(verbosity=2)
