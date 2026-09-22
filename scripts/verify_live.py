#!/usr/bin/env python3
"""One user-authorized live call through an already running local gateway.
Never prints/saves credentials or prompt/response bodies. Writes sanitized evidence.
"""
import argparse, datetime, json, pathlib, sqlite3, subprocess, urllib.request

def main():
    ap=argparse.ArgumentParser();ap.add_argument('--config',required=True);ap.add_argument('--disable-thinking',action='store_true');ap.add_argument('--output',required=True);ap.add_argument('--openai-token-limit-field',choices=['max_tokens','max_completion_tokens'],default='max_tokens');args=ap.parse_args()
    c=json.loads(pathlib.Path(args.config).read_text());protocol=c['protocol']
    if protocol not in ('openai','anthropic'):raise SystemExit('Unsupported protocol')
    ref=c['access_token_keychain']
    secret=subprocess.run(['security','find-generic-password','-s',ref['service'],'-a',ref['account'],'-w'],capture_output=True)
    if secret.returncode:raise SystemExit('Gateway Keychain item unavailable')
    token=secret.stdout.decode().strip()
    body={'model':c['model'],'messages':[{'role':'user','content':'Reply with OK.'}],args.openai_token_limit_field:16}
    if args.disable_thinking:body['thinking']={'type':'disabled'}
    endpoint='/v1/chat/completions'
    request=urllib.request.Request('http://'+c['listen_addr']+endpoint,json.dumps(body).encode(),headers={'Content-Type':'application/json','Authorization':'Bearer '+token})
    try:
        with urllib.request.urlopen(request,timeout=130) as response:
            request_id=response.headers.get('X-TideMux-Request-ID');payload=json.load(response)
    except Exception:
        raise SystemExit('Live request failed; inspect tidemux billing --details or tidemux doctor --diagnostics for a safe error code. No raw response printed.')
    db=sqlite3.connect('file:'+str(pathlib.Path(c['ledger_path']).resolve())+'?mode=ro',uri=True)
    row=db.execute('SELECT record_json FROM request_audit WHERE id=?',(request_id,)).fetchone();db.close()
    if not row:raise SystemExit('No matching audit record')
    audit=json.loads(row[0]);u=payload.get('usage',{})
    input_count=u.get('prompt_tokens');output_count=u.get('completion_tokens')
    if audit['status']!='ok' or input_count is None or output_count is None or input_count!=audit['input_tokens'] or output_count!=audit['output_tokens']:raise SystemExit('Live usage reconciliation failed')
    if audit['estimated_cost'] is None:raise SystemExit('Usage reconciled, but estimated cost is unknown. Configure verified model pricing and repeat only when authorized.')
    p=audit['price_snapshot'];cache_read=audit.get('cache_read_tokens') or 0
    input_cache_miss=input_count-cache_read
    expected=(output_count*p['output_per_million']+cache_read*p['input_cache_hit_per_million']+input_cache_miss*p['input_cache_miss_per_million'])/1e6
    if abs(expected-audit['estimated_cost'])>1e-10:raise SystemExit('Cost arithmetic mismatch')
    output=pathlib.Path(args.output)
    if output.exists():raise SystemExit('Evidence already exists; will not overwrite')
    output.parent.mkdir(parents=True,exist_ok=True)
    evidence={'verified_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'kind':'live-provider','protocol':protocol,'request_id':request_id,'usage_reconciled':True,'cost_arithmetic_reconciled':True,'audit':audit}
    with output.open('x') as f:json.dump(evidence,f,indent=2);f.write('\n')
    output.chmod(0o600);print('Live usage and cost arithmetic reconciled; sanitized evidence saved.')
if __name__=='__main__':main()
