#!/usr/bin/env python3
"""Verify automatic provider application in one isolated packaged process."""
import argparse
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler
import json
import os
from pathlib import Path
import pty
import select
import signal
import sqlite3
import subprocess
import tempfile
import threading
import time
from urllib.error import HTTPError
from urllib.request import Request, urlopen
from test_runtime_logs_package import Server, free_port, GATEWAY_KEY

KEY = 'private-hot-reload-key'
PROMPT = 'private-hot-reload-prompt'
SESSION = 'private-hot-reload-session'

class Upstream(BaseHTTPRequestHandler):
    def log_message(self, *_): pass
    def reply(self, value):
        data = json.dumps(value).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)
    def do_GET(self):
        self.reply({'object':'list','data':[{'id':m,'type':'model'} for m in ('custom-model','other-model')],'has_more':False})
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        with self.server.lock:
            self.server.calls.append((self.headers.get('Authorization', self.headers.get('x-api-key','')), body['model']))
        if body.get('stream'):
            self.send_response(200)
            self.send_header('Content-Type','text/event-stream')
            self.end_headers()
            self.wfile.write(b'data: {"choices":[{"index":0,"delta":{"content":"held"}}]}\n\n')
            self.wfile.flush()
            self.server.started.set()
            self.server.release.wait(90)
            self.wfile.write(b'data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}\n\ndata: [DONE]\n\n')
            self.wfile.flush()
        else:
            self.reply({'id':'test','object':'chat.completion','model':body['model'],'choices':[{'index':0,'message':{'role':'assistant','content':'hi'},'finish_reason':'stop'}],'usage':{'prompt_tokens':3,'completion_tokens':2}})

def call(url, path, body=None):
    headers={'Authorization':'Bearer '+GATEWAY_KEY,'Content-Type':'application/json','X-TideMux-Session-ID':SESSION}
    req=Request(url+path, None if body is None else json.dumps(body).encode(),headers)
    try: response=urlopen(req,timeout=100)
    except HTTPError as error: response=error
    return response

def query(url,path,body=None):
    with call(url,path,body) as response: return response.status,response.read().decode()

def body(model,stream=False):
    return {'model':model,'max_tokens':32,'stream':stream,'messages':[{'role':'user','content':PROMPT}]}

def wait_for(predicate,label,seconds=15):
    deadline=time.monotonic()+seconds
    while time.monotonic()<deadline:
        if predicate(): return
        time.sleep(.1)
    raise RuntimeError(label+' did not apply automatically within '+str(seconds)+' seconds')

def cli(binary,env,config,args,secret=None):
    argv=[str(binary),'provider']+args+['--config',str(config)]
    if args[:2]==['key','remove']:argv=[str(binary),'provider']+args[:3]+['--config',str(config)]+args[3:]
    if secret is None:
        result=subprocess.run(argv,env=env,capture_output=True,text=True,timeout=15)
        if result.returncode: raise RuntimeError('CLI failed: '+result.stdout+result.stderr)
        return result.stdout
    pid,fd=pty.fork()
    if pid==0: os.execve(str(binary),argv,env)
    captured=b''; sent=False; status=None; deadline=time.monotonic()+20
    try:
        while time.monotonic()<deadline:
            if select.select([fd],[],[],.1)[0]:
                try: captured+=os.read(fd,8192)
                except OSError: pass
                if not sent and b'(hidden):' in captured:
                    time.sleep(.05);os.write(fd,secret.encode()+b'\n');sent=True
            done,status0=os.waitpid(pid,os.WNOHANG)
            if done: status=status0;break
        if status is None:
            os.kill(pid,signal.SIGKILL);os.waitpid(pid,0);raise RuntimeError('CLI PTY timed out')
    finally: os.close(fd)
    if status or not sent or secret.encode() in captured: raise RuntimeError('CLI failed or echoed key')
    return captured.decode()

def write_config(path,value):
    tmp=path.with_suffix('.new');tmp.write_text(json.dumps(value));tmp.replace(path)

def main():
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--binary',required=True,type=Path);args=parser.parse_args()
    binary=args.binary.resolve()
    with tempfile.TemporaryDirectory(prefix='tidemux-hot-reload.') as temporary:
        root=Path(temporary);fake=root/'bin';fake.mkdir();db=root/'secrets.json'
        db.write_text(json.dumps({'test.gateway/local':GATEWAY_KEY,'test.provider/local':KEY}))
        security=fake/'security'
        security.write_text('''#!/usr/bin/env python3
import sys,os,json,shlex
p=os.environ['TIDEMUX_TEST_KEYCHAIN_DB'];d=json.load(open(p));a=sys.argv[1:]
if a==['show-keychain-info']:sys.exit(0)
if a==['-i']:a=shlex.split(sys.stdin.read());k=a[a.index('-s')+1]+'/'+a[a.index('-a')+1];d[k]=bytes.fromhex(a[a.index('-X')+1]).decode();json.dump(d,open(p,'w'))
elif a[0]=='find-generic-password':
 k=a[a.index('-s')+1]+'/'+a[a.index('-a')+1]
 if k not in d:sys.exit(1)
 print(d[k])
elif a[0]=='delete-generic-password':k=a[a.index('-s')+1]+'/'+a[a.index('-a')+1];d.pop(k,None);json.dump(d,open(p,'w'))
else:sys.exit(1)
''');security.chmod(0o700)
        env=dict(os.environ,PATH=str(fake)+':'+os.environ.get('PATH',''),HOME=str(root/'home'),TIDEMUX_TEST_KEYCHAIN_DB=str(db))
        upstream=Server(('127.0.0.1',0),Upstream);upstream.lock=threading.Lock();upstream.calls=[];upstream.started=threading.Event();upstream.release=threading.Event()
        threading.Thread(target=upstream.serve_forever,daemon=True).start()
        endpoint=f'http://127.0.0.1:{upstream.server_port}/v1';port=free_port();url=f'http://127.0.0.1:{port}'
        config=root/'config.json';ledger=root/'ledger.db'
        prices={'currency':'USD','source':'mock','version':'1','input_cache_miss_per_million':1,'input_cache_hit_per_million':.1,'output_per_million':2}
        value={'listen_addr':f'127.0.0.1:{port}','max_in_flight':8,'ledger_path':str(ledger),'access_token_keychain':{'service':'test.gateway','account':'local'},'providers':{'original':{'protocol':'openai','base_url':endpoint,'upstream_keychain':{'service':'test.provider','account':'local'},'supported_models':['custom-model'],'prices':{'custom-model':prices},'budget':{'currency':'USD','five_hour_limit':100,'weekly_limit':100,'mode':'hard','alert_threshold':.8}}}}
        write_config(config,value)
        process=None
        try:
            with (root/'stdout').open('w') as stdout,(root/'stderr').open('w') as stderr:
                process=subprocess.Popen([str(binary),'serve','--config',str(config)],env=env,stdout=stdout,stderr=stderr)
                wait_for(lambda: 'listening' in (root/'stdout').read_text(),'startup',10)
                pid=process.pid
                with call(url,'/v1/chat/completions',body('original/custom-model',True)) as held:
                    if not upstream.started.wait(5):raise RuntimeError('SSE did not start')
                    while held.readline().strip():pass
                    output=cli(binary,env,config,['add',endpoint,'--name','added','--protocol','openai','--model','custom-model'],KEY+'-added')
                    wait_for(lambda: 'added/custom-model' in query(url,'/v1/models')[1],'CLI-added provider')
                    if 'pending' not in output and 'applied' not in output:raise RuntimeError('CLI has no applied/pending status')
                    for path in ('/v1/chat/completions','/v1/messages'):
                        if query(url,path,body('added/custom-model'))[0]!=200:raise RuntimeError('added provider not callable in both protocols')
                    with ThreadPoolExecutor(max_workers=8) as pool:
                        if any(status!=200 for status,_ in pool.map(lambda _:query(url,'/v1/chat/completions',body('added/custom-model')),range(16))):raise RuntimeError('concurrent requests failed')
                    cli(binary,env,config,['update','added','--model','other-model'])
                    wait_for(lambda:'added/other-model' in query(url,'/v1/models')[1] and 'added/custom-model' not in query(url,'/v1/models')[1],'model scope')
                    if query(url,'/v1/chat/completions',body('added/custom-model'))[0]!=404:raise RuntimeError('old scope still routable')
                    cli(binary,env,config,['key','add','added'],KEY+'-extra')
                    wait_for(lambda: query(url,'/tidemux/config-status')[0]==200 and json.loads(query(url,'/tidemux/config-status')[1])['status']=='applied','key addition')
                    start=len(upstream.calls)
                    for _ in range(4):query(url,'/v1/chat/completions',body('added/other-model'))
                    if not any(KEY+'-extra' in key for key,_ in upstream.calls[start:]):raise RuntimeError('new key unused')
                    cli(binary,env,config,['key','remove','added','--yes','2'])
                    wait_for(lambda:json.loads(query(url,'/tidemux/config-status')[1])['status']=='applied','key removal')
                    start=len(upstream.calls)
                    for _ in range(3):query(url,'/v1/chat/completions',body('added/other-model'))
                    if any(KEY+'-extra' in key for key,_ in upstream.calls[start:]):raise RuntimeError('removed key reused')
                    cli(binary,env,config,['update','added','--rotate-key'],KEY+'-rotated')
                    wait_for(lambda:json.loads(query(url,'/tidemux/config-status')[1])['status']=='applied','rotation')
                    query(url,'/v1/chat/completions',body('added/other-model'))
                    if KEY+'-rotated' not in upstream.calls[-1][0]:raise RuntimeError('rotation unused')
                    # Same-reference Keychain rotation is detected without a config write.
                    secrets=json.loads(db.read_text());ref=json.loads(config.read_text())['providers']['added']['upstream_keychains'][0]
                    secrets[ref['service']+'/'+ref['account']]=KEY+'-same-ref';db.write_text(json.dumps(secrets))
                    wait_for(lambda: query(url,'/v1/chat/completions',body('added/other-model'))[0]==200 and KEY+'-same-ref' in upstream.calls[-1][0],'same-reference rotation')
                    good=json.loads(config.read_text());bad=json.loads(config.read_text());bad['providers']['added']['base_url']='http://127.0.0.1:1/v1';write_config(config,bad)
                    wait_for(lambda:json.loads(query(url,'/tidemux/config-status')[1])['status']=='pending','failed probe')
                    if query(url,'/v1/chat/completions',body('added/other-model'))[0]!=200:raise RuntimeError('last valid provider lost')
                    bad=json.loads(config.read_text());bad['providers']['added']['upstream_keychains'][0]['account']='missing';write_config(config,bad);time.sleep(1.2)
                    if query(url,'/v1/chat/completions',body('original/custom-model'))[0]!=200:raise RuntimeError('unrelated provider unavailable')
                    config.write_text('{bad-json');time.sleep(1.2)
                    if query(url,'/v1/chat/completions',body('added/other-model'))[0]!=200:raise RuntimeError('invalid config replaced snapshot')
                    write_config(config,good);wait_for(lambda:json.loads(query(url,'/tidemux/config-status')[1])['status']=='applied','restored config')
                    # Change prices/budget, then remove original while its SSE is admitted.
                    changed=json.loads(config.read_text());changed['providers']['original']['prices']['custom-model']['output_per_million']=200;changed['providers']['original']['budget']['currency']='EUR';write_config(config,changed)
                    wait_for(lambda:json.loads(query(url,'/tidemux/config-status')[1])['status']=='applied','pricing/budget snapshot')
                    cli(binary,env,config,['remove','original','--yes'])
                    wait_for(lambda:'original/custom-model' not in query(url,'/v1/models')[1],'removal')
                    if query(url,'/v1/chat/completions',body('original/custom-model'))[0]!=404:raise RuntimeError('removed provider routable')
                    upstream.release.set();tail=held.read().decode()
                    if '[DONE]' not in tail or 'event: error' in tail:raise RuntimeError('reload interrupted SSE')
                if process.pid!=pid or process.poll() is not None:raise RuntimeError('gateway restarted')
                with sqlite3.connect(ledger) as connection:
                    rows=connection.execute("SELECT record_json FROM request_audit WHERE status='ok'").fetchall()
                    records=[json.loads(row[0]) for row in rows]
                    original=[r for r in records if r.get('provider_ref')=='original' and r.get('stream')]
                    # Audit uses exactly one successful record per dispatched request.
                    if len(records)!=len(upstream.calls):raise RuntimeError('dispatch/settlement counts differ')
                    charges=connection.execute("SELECT currency,charged_amount,state FROM budget_charges WHERE provider_scope='original'").fetchall()
                    if not charges or any(c!='USD' or state!='settled' or amount>.001 for c,amount,state in charges):raise RuntimeError('admitted budget/pricing snapshot changed: '+str(charges))
                process.send_signal(signal.SIGTERM);process.wait(timeout=10)
                if process.returncode:raise RuntimeError('shutdown failed')
                logs=(root/'stderr').read_text()
                if any(marker in logs for marker in (KEY,PROMPT,SESSION)):raise RuntimeError('reload log leaked private data')
        finally:
            upstream.release.set()
            if process and process.poll() is None:process.kill();process.wait()
            upstream.shutdown();upstream.server_close()
    print(json.dumps({'binary':str(binary),'one_process':True,'cli_add_both_protocols':True,'scope_update_remove_keys':True,'failed_reload_preserves_snapshot':True,'held_sse_pricing_budget':True,'concurrent_unique_settlement':True,'privacy':True}))

if __name__=='__main__':main()
