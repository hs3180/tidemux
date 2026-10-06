#!/usr/bin/env python3
"""Verify a real installed ccusage against an isolated TideMux package + ledger."""
import argparse
from collections import Counter
from datetime import datetime
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
METRICS = ('input_tokens','output_tokens','cache_read_tokens','cache_write_tokens','estimated_cost','matched_supplier_amount')

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

def statements(ledger):
    with sqlite3.connect(ledger) as db:
        return {row[0]:{'revision':row[1],'lines':row[2],'amount':row[3]}
                for row in db.execute("SELECT request_id,MAX(statement_id),COUNT(*),SUM(amount) FROM reconciliation_statements WHERE status='matched' GROUP BY request_id")}

def record_oracle(audit, supplier):
    # The ledger is authoritative. Provider tokens times a price are estimates;
    # only an explicitly matched bill belongs in the independent supplier column.
    usage_source=audit.get('usage_source') or 'unknown'
    local=audit.get('cost_source','').startswith('local_estimated')
    if usage_source=='unknown' and local:usage_source='local_estimate'
    cost_source='unknown'
    if audit.get('estimated_cost') is not None:
        cost_source='local_content_estimate' if local or usage_source=='local_estimate' else ('historical_estimate' if usage_source=='unknown' else 'estimated_from_provider_usage')
    price=audit.get('price_snapshot')
    return {'schema_version':1,'source':'tidemux','request_id':audit['id'],
            'revision':supplier.get('revision',0),'session_group':audit.get('session_group') or None,
            'provider':audit.get('provider_ref',''),'model':audit['model'],'protocol':audit['protocol'],'outcome':audit['status'],
            'usage_source':usage_source,**{name:audit.get(name) for name in METRICS if name!='matched_supplier_amount'},
            'currency':audit.get('currency') or None,'cost_source':cost_source,
            'price_source':'audit_price_snapshot' if price else 'unknown','price_version':(price.get('version') or None) if price else None,
            'supplier_amount':supplier.get('amount'),'supplier_statement_lines':supplier.get('lines',0),
            'supplier_source':'matched_supplier_statement' if supplier.get('lines',0) else 'unknown'}

def expected(rows, grouping, ledger):
    supplier=statements(ledger)
    groups={}
    for row in rows:
        group=row.get('session_group') if grouping=='session' else None
        currency=row.get('currency') or None
        groups.setdefault((group,currency),[]).append(row)
    output={}
    for key,values in groups.items():
        result={'requests':len(values)}
        for name in METRICS:
            numbers=[supplier.get(r['id'],{}).get('amount') if name=='matched_supplier_amount' else r.get(name) for r in values]
            known=[n for n in numbers if n is not None]
            result[name]={'total':sum(known) if len(known)==len(numbers) else None,
                          'known_subtotal':sum(known) if known else None,'known_requests':len(known),'unknown_requests':len(numbers)-len(known)}
        records=[record_oracle(row,supplier.get(row['id'],{})) for row in values]
        for name in ('usage','cost','supplier'):
            result[name+'_sources']=dict(Counter(row[name+'_source'] for row in records))
        result['price_sources']=sorted({row['price_source'] for row in records})
        result['price_versions']=sorted({row['price_version'] for row in records if row['price_version'] is not None})
        result['supplier_statement_lines']=sum(row['supplier_statement_lines'] for row in records)
        result['providers']=sorted({row['provider'] for row in records})
        result['models']=sorted({row['model'] for row in records})
        result['outcomes']=dict(Counter(row['outcome'] for row in records))
        output[key]=result
    return output

def verify(report,rows,ledger,grouping):
    assert (report['schema_version'],report['source'],report['grouping'],report['timezone'])==(1,'tidemux',grouping,'UTC')
    assert report['retained_records']==len(rows),(report['retained_records'],len(rows))
    actual={(r['group'],r['currency']):r for r in report['rows']}
    wanted=expected(rows,grouping,ledger)
    assert actual.keys()==wanted.keys(),(actual.keys(),wanted.keys())
    for key,row in wanted.items():
        assert actual[key]['requests']==row['requests']
        for column in METRICS:
            for field,value in row[column].items():
                got=actual[key][column][field]
                assert got==value or (got is not None and value is not None and math.isclose(got,value,rel_tol=1e-12)),(key,column,field,got,value)
        for column in ('usage_sources','cost_sources','supplier_sources','price_sources','price_versions','supplier_statement_lines','providers','models','outcomes'):
            assert actual[key][column]==row[column],(key,column,actual[key][column],row[column])

def verify_records(output,rows,ledger):
    latest={};source_ids=set()
    for path in sorted(output.glob('usage-*.jsonl')):
        for line in path.read_text().splitlines():
            record=json.loads(line);source_ids.add(record['source_id'])
            previous=latest.get(record['request_id'])
            if previous is None or record['revision']>previous['revision']:latest[record['request_id']]=record
            elif record['revision']==previous['revision']:assert record==previous,'conflicting record revision'
    assert len(source_ids)==1 and all(len(value)==64 and all(c in '0123456789abcdef' for c in value) for value in source_ids)
    assert latest.keys()=={row['id'] for row in rows},'exported identity coverage differs from committed ledger'
    supplier=statements(ledger)
    for row in rows:
        actual=latest[row['id']];wanted=record_oracle(row,supplier.get(row['id'],{}))
        for name,value in wanted.items():assert actual[name]==value,(row['id'],name,actual[name],value)
        assert round(datetime.fromisoformat(actual['timestamp'].replace('Z','+00:00')).timestamp()*1000)==row['timestamp_ms']
    return list(latest.values())

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
            assert len(all_rows)==12
            final_session=report('session');final_aggregate=report('aggregate')
            verify(final_session,all_rows,ledger,'session');verify(final_aggregate,all_rows,ledger,'aggregate')
            final_records=verify_records(output,all_rows,ledger)
            # Replay after fault recovery must preserve all 12 records and every
            # source/coverage/amount field, rather than only succeeding as a CLI.
            cli('usage','export','--backfill')
            replay_session=report('session');replay_aggregate=report('aggregate')
            verify(replay_session,all_rows,ledger,'session');verify(replay_aggregate,all_rows,ledger,'aggregate')
            assert replay_aggregate['replayed_records']>final_aggregate['replayed_records']
            assert replay_session['rows']==final_session['rows'] and replay_aggregate['rows']==final_aggregate['rows']
            final_records=verify_records(output,all_rows,ledger)
            # An explicitly selected TideMux source ignores a client's filename
            # and rejects client records disguised as TideMux usage input.
            foreign=output/'client-owned.jsonl'
            foreign.write_text(json.dumps({'request_id':all_rows[0]['id'],'usage':{'input_tokens':999},'costUSD':999})+'\n')
            try:assert report('aggregate')==replay_aggregate
            finally:foreign.unlink()
            disguised=output/'usage-99999999999999999999.jsonl'
            disguised.write_text(json.dumps(dict(final_records[0],source='claude'))+'\n')
            try:
                rejected=subprocess.run([str(consumer),'tidemux','aggregate','--path',str(output),'--json'],capture_output=True,text=True,timeout=20)
                assert rejected.returncode and 'invalid TideMux usage record contract' in rejected.stderr
            finally:disguised.unlink()
            assert report('aggregate')==replay_aggregate
            for name,data in [('final-session.json',replay_session),('final-aggregate.json',replay_aggregate),('final-records.json',final_records),
                              ('final-ledger-oracle.json',[dict(group=group,currency=currency,**expected_row) for (group,currency),expected_row in expected(all_rows,'session',ledger).items()])]:
                (evidence/name).write_text(json.dumps(data,indent=2)+'\n')
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
            summary={'result':'passed','fixture_requests':len(audits(ledger)),'final_consumer_records':replay_aggregate['retained_records'],'default_off':True,'real_ccusage':str(consumer),'unknown_and_multicurrency':True,'session_restart':True,'supplier_revision_and_replay':True,'half_tail_recovery':True,'export_and_identity_fault_isolation':True,'fault_recovery_catchup':True,'provenance_checked':True,'record_contract_checked':True,'source_selection':True,'privacy':True}
            (evidence/'result.json').write_text(json.dumps(summary,indent=2)+'\n');print(json.dumps(summary))
        finally:stop();upstream.release.set();upstream.shutdown();upstream.server_close()

if __name__=='__main__':main()
