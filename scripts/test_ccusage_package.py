#!/usr/bin/env python3
"""Verify a real installed ccusage against an isolated TideMux package + ledger."""
import argparse
from http.server import BaseHTTPRequestHandler
import json
import math
import os
from pathlib import Path
import shutil
import signal
import sqlite3
import subprocess
import tempfile
import threading
import time
from urllib.request import Request, urlopen
from urllib.error import HTTPError, URLError
from test_runtime_logs_package import Server, free_port, GATEWAY_KEY, PROVIDER_KEY

PROMPT = 'usage-private-prompt-sentinel'
SESSION = 'usage-private-session-sentinel'
RESPONSE = 'usage-private-response-sentinel'

class Mock(BaseHTTPRequestHandler):
    def log_message(self, *_): pass
    def write(self, status, value):
        data = json.dumps(value).encode()
        self.send_response(status); self.send_header('Content-Type','application/json')
        self.send_header('Content-Length',str(len(data))); self.end_headers(); self.wfile.write(data)
    def do_GET(self): self.write(200,{'object':'list','data':[{'id':'fixture'}]})
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        if body.get('stream'):
            self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
            self.wfile.write(b'data: {"choices":[{"index":0,"delta":{"content":"usage-private-response-sentinel"}}]}\n\n');self.wfile.flush()
            self.server.started.set();self.server.release.wait(10)
            try: self.wfile.write(b'data: [DONE]\n\n');self.wfile.flush()
            except (BrokenPipeError,ConnectionResetError): pass
            return
        if '/error/' in self.path:
            self.write(403,{'error':{'message':RESPONSE,'code':'private-error-sentinel'}});return
        value={'id':'fixture','object':'chat.completion','model':'fixture','choices':[{'index':0,'message':{'role':'assistant','content':RESPONSE},'finish_reason':'stop'}]}
        if '/unknown/' not in self.path:
            value['usage']={'prompt_tokens':7,'completion_tokens':2,'prompt_tokens_details':{'cached_tokens':3}}
        if '/partial/' in self.path: value['usage']={'prompt_tokens':5}
        self.write(200,value)

def wait_for(predicate,label):
    deadline=time.monotonic()+20
    while time.monotonic()<deadline:
        if predicate():return
        time.sleep(.1)
    raise RuntimeError(label+' timed out')

def audits(path):
    with sqlite3.connect(path) as db:
        return [json.loads(row[0]) for row in db.execute('SELECT record_json FROM request_audit ORDER BY rowid')]

def expected(rows, grouping, ledger):
    with sqlite3.connect(ledger) as db:
        supplier={row[0]:row[1] for row in db.execute("SELECT request_id,SUM(amount) FROM reconciliation_statements WHERE status='matched' GROUP BY request_id")}
    groups={}
    for row in rows:
        group=row.get('session_group') if grouping=='session' else None
        currency=row.get('currency') or None
        groups.setdefault((group,currency),[]).append(row)
    output={}
    for key,values in groups.items():
        result={'requests':len(values)}
        for name in ('input_tokens','output_tokens','cache_read_tokens','cache_write_tokens','estimated_cost','matched_supplier_amount'):
            numbers=[supplier.get(r['id']) if name=='matched_supplier_amount' else r.get(name) for r in values]
            known=[n for n in numbers if n is not None]
            result[name]={'total':sum(known) if len(known)==len(numbers) else None,
                          'known_subtotal':sum(known) if known else None,'known_requests':len(known),'unknown_requests':len(numbers)-len(known)}
        output[key]=result
    return output

def verify(report,rows,ledger,grouping):
    assert report['retained_records']==len(rows),(report['retained_records'],len(rows))
    actual={(r['group'],r['currency']):r for r in report['rows']}
    wanted=expected(rows,grouping,ledger)
    assert actual.keys()==wanted.keys(),(actual.keys(),wanted.keys())
    for key,row in wanted.items():
        assert actual[key]['requests']==row['requests']
        for column,coverage in row.items():
            if column=='requests':continue
            for field,value in coverage.items():
                got=actual[key][column][field]
                assert got==value or (got is not None and value is not None and math.isclose(got,value,rel_tol=1e-12)),(key,column,field,got,value)

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary',required=True,type=Path);parser.add_argument('--ccusage',required=True,type=Path)
    parser.add_argument('--evidence-dir',required=True,type=Path);args=parser.parse_args()
    binary=args.binary.resolve();consumer=args.ccusage.resolve();evidence=args.evidence_dir.resolve();evidence.mkdir(parents=True,exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='tidemux-usage-package.') as temporary:
        root=Path(temporary);fake=root/'bin';fake.mkdir()
        security=fake/'security';security.write_text('#!/usr/bin/env python3\nimport sys\na=sys.argv[1:]\nif a==["show-keychain-info"]:sys.exit(0)\nif a[0]=="find-generic-password":print("local-gateway-key" if a[a.index("-s")+1]=="test.gateway" else "local-provider-key");sys.exit(0)\nsys.exit(1)\n');security.chmod(0o700)
        env=dict(os.environ,PATH=str(fake)+':'+os.environ.get('PATH',''),HOME=str(root/'home'))
        upstream=Server(('127.0.0.1',0),Mock);upstream.started=threading.Event();upstream.release=threading.Event()
        threading.Thread(target=upstream.serve_forever,daemon=True).start()
        port=free_port();url=f'http://127.0.0.1:{port}';ledger=root/'ledger.db';config=root/'config.json';output=root/'usage'
        prices=lambda currency:{'currency':currency,'source':'fixture','version':'fixture-v1','input_cache_miss_per_million':1,'input_cache_hit_per_million':.1,'output_per_million':2}
        providers={}
        for name in ('usd','eur','unknown','partial','error'):
            providers[name]={'protocol':'openai','base_url':f'http://127.0.0.1:{upstream.server_port}/{name}/v1','upstream_keychain':{'service':'test.provider','account':'local'},'supported_models':['fixture']}
            if name in ('usd','eur'):providers[name]['prices']={'fixture':prices(name.upper())}
            if name=='usd':providers[name]['budget']={'currency':'USD','five_hour_limit':10,'weekly_limit':10,'alert_threshold':.8,'mode':'hard'}
        value={'listen_addr':f'127.0.0.1:{port}','max_in_flight':8,'ledger_path':str(ledger),'access_token_keychain':{'service':'test.gateway','account':'local'},'providers':providers,'reconciliation':{'poll_interval_seconds':1}}
        config.write_text(json.dumps(value));process=None
        def cli(*arguments):
            result=subprocess.run([str(binary),*arguments,'--config',str(config)],env=env,capture_output=True,text=True,timeout=30)
            if result.returncode:raise RuntimeError('local CLI failed: '+result.stderr)
            return result.stdout
        def start():
            nonlocal process
            process=subprocess.Popen([str(binary),'serve','--config',str(config)],env=env,stdout=(root/'stdout').open('a'),stderr=(root/'stderr').open('a'))
            wait_for(lambda:process.poll() is not None or (root/'stdout').exists() and 'listening' in (root/'stdout').read_text(),'startup')
            if process.poll() is not None:raise RuntimeError('fixture startup failed: '+(root/'stderr').read_text())
            def ready():
                if process.poll() is not None:raise RuntimeError('fixture startup failed: '+(root/'stderr').read_text())
                try:
                    with urlopen(Request(url+'/v1/models',headers={'Authorization':'Bearer '+GATEWAY_KEY}),timeout=1) as response:return response.status==200
                except URLError:return False
            wait_for(ready,'gateway readiness')
        def stop():
            nonlocal process
            if process is not None:process.send_signal(signal.SIGTERM);process.wait(timeout=10);process=None
        def request(provider,session=None,protocol='openai',stream=False):
            body={'model':provider+'/fixture','max_tokens':32,'messages':[{'role':'user','content':PROMPT}],'stream':stream}
            headers={'Authorization':'Bearer '+GATEWAY_KEY,'Content-Type':'application/json'}
            if session is not None:
                if protocol=='anthropic':body['metadata']={'user_id':json.dumps({'session_id':session})}
                else:headers['X-TideMux-Session-ID']=session
            req=Request(url+('/v1/messages' if protocol=='anthropic' else '/v1/chat/completions'),json.dumps(body).encode(),headers)
            try:response=urlopen(req,timeout=20)
            except HTTPError as error:response=error
            if stream:return response
            with response:return response.status,response.read()
        def report(kind):
            result=subprocess.run([str(consumer),'tidemux',kind,'--path',str(output),'--json'],capture_output=True,text=True,timeout=20)
            if result.returncode:raise RuntimeError('actual ccusage failed: '+result.stderr)
            return json.loads(result.stdout)
        try:
            start();request('usd',SESSION);stop()
            assert not output.exists() and not Path(str(ledger)+'.usage-key').exists()
            assert all(not r.get('session_group') for r in audits(ledger))
            cli('usage','configure','--enable');start()
            for provider,session in [('usd',SESSION),('usd',SESSION),('eur',SESSION),('unknown',SESSION),('partial',None),('error',SESSION)]:
                code,_=request(provider,session);assert code==(502 if provider=='error' else 200),(provider,code)
            assert request('usd',SESSION,'anthropic')[0]==200
            stream=request('unknown',SESSION,stream=True);assert upstream.started.wait(5)
            stream.readline();stream.close();upstream.release.set()
            wait_for(lambda:any(r['status']=='canceled' for r in audits(ledger)),'canceled audit')
            stop();cli('usage','export')
            rows=audits(ledger);session_report=report('session');aggregate=report('aggregate')
            verify(session_report,rows,ledger,'session');verify(aggregate,rows,ledger,'aggregate')
            groups={r.get('session_group') for r in rows if r.get('session_group')};assert len(groups)==2,groups
            start();assert request('usd',SESSION)[0]==200;stop();cli('usage','export')
            rows=audits(ledger);assert {r.get('session_group') for r in rows if r.get('session_group')}==groups
            target=next(r for r in rows if r.get('currency')=='EUR');statements=root/'statements';statements.mkdir(exist_ok=True)
            (statements/'fixture.csv').write_text(f"period_start,period_end,currency,amount,request_id,model\n{target['timestamp_ms']-1},{target['timestamp_ms']+1},EUR,2.25,{target['id']},fixture\n")
            start()
            def matched():
                with sqlite3.connect(ledger) as db:return db.execute("SELECT COUNT(*) FROM reconciliation_statements WHERE status='matched'").fetchone()[0]==1
            wait_for(matched,'matched supplier revision');stop();cli('usage','export')
            rows=audits(ledger);before=report('aggregate');verify(before,rows,ledger,'aggregate')
            cli('usage','export','--backfill');after=report('aggregate');verify(after,rows,ledger,'aggregate')
            assert after['replayed_records']>before['replayed_records']
            checkpoint=output/'checkpoint.json';state=json.loads(checkpoint.read_text());checkpoint.unlink()
            latest=sorted(output.glob('usage-*.jsonl'))[-1]
            with latest.open('ab') as f:f.write(b'{"crash_half":')
            cli('usage','export');recovered=report('aggregate');verify(recovered,rows,ledger,'aggregate')
            (evidence/'session.json').write_text(json.dumps(report('session'),indent=2)+'\n');(evidence/'aggregate.json').write_text(json.dumps(recovered,indent=2)+'\n')
            checkpoint.write_text(json.dumps(state));checkpoint.chmod(0o600)
            blocked=root/'blocked';blocked.write_text('output fault');value=json.loads(config.read_text());value['usage_log']['directory']=str(blocked);config.write_text(json.dumps(value))
            start();assert request('usd',SESSION)[0]==200
            wait_for(lambda:Path(str(ledger)+'.usage-status.json').exists() and json.loads(Path(str(ledger)+'.usage-status.json').read_text()).get('error_code')=='usage_output_unavailable','output fault isolation');stop()
            value['usage_log']['directory']=str(output);config.write_text(json.dumps(value));key=Path(str(ledger)+'.usage-key');saved=key.read_bytes();key.write_bytes(b'invalid-key');key.chmod(0o600)
            start();assert request('usd',SESSION)[0]==200;stop();assert audits(ledger)[-1].get('session_group') is None
            key.write_bytes(saved);cli('usage','export')
            all_rows=audits(ledger)
            with sqlite3.connect(ledger) as db:
                settled=db.execute("SELECT audit_id,charged_amount FROM budget_charges WHERE provider_scope='usd' AND state='settled'").fetchall()
                assert len(settled)==sum(r.get('provider_ref')=='usd' for r in all_rows)
                by_id={r['id']:r for r in all_rows}
                assert all(math.isclose(amount,by_id[identity]['estimated_cost'],rel_tol=1e-12) for identity,amount in settled)
                assert db.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0]==sum(len(r['events']) for r in all_rows)
            blob=b''.join(p.read_bytes() for p in output.iterdir() if p.is_file())+(root/'stdout').read_bytes()+(root/'stderr').read_bytes()
            for sentinel in (PROMPT,SESSION,RESPONSE,GATEWAY_KEY,PROVIDER_KEY,'private-error-sentinel'):
                assert sentinel.encode() not in blob,'private data leaked'
            for p in output.iterdir():assert p.stat().st_mode&0o077==0
            summary={'result':'passed','fixture_requests':len(audits(ledger)),'default_off':True,'real_ccusage':str(consumer),'unknown_and_multicurrency':True,'session_restart':True,'supplier_revision_and_replay':True,'half_tail_recovery':True,'export_and_identity_fault_isolation':True,'privacy':True}
            (evidence/'result.json').write_text(json.dumps(summary,indent=2)+'\n');print(json.dumps(summary))
        finally:stop();upstream.release.set();upstream.shutdown();upstream.server_close()

if __name__=='__main__':main()
