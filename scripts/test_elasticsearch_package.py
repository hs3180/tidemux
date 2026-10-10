#!/usr/bin/env python3
"""Index packaged runtime logs into an explicitly supplied isolated loopback ES.

Requires Docker, Elasticsearch 8.19.5 and the official Logstash 8.19.5 image.
Creates only a fresh tidemux-smoke-* index namespace, and retains evidence.
"""
import argparse
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler
import json
import os
from pathlib import Path
import re
import shutil
import ssl
import subprocess
import time
from urllib.parse import urlparse
from urllib.request import Request, urlopen

from runtime_event_checks import validate_runtime_events
from test_client_reliability_package import running_gateway, request, read_response, start_frame
from test_runtime_logs_package import GATEWAY_KEY, PROVIDER_KEY, REQUEST_SENTINEL, SESSION_SENTINEL

ROOT=Path(__file__).resolve().parents[1]
IMAGE='docker.elastic.co/logstash/logstash:8.19.5'

def event_time(value):
    # Older Python versions accept only three/six fraction digits, while Go's
    # RFC3339Nano output omits trailing zeroes. Compare at microsecond precision.
    normalized=re.sub(r'\.(\d+)(?=Z|[+-]\d{2}:)',lambda match:'.'+match[1][:6].ljust(6,'0'),value)
    return datetime.fromisoformat(normalized.replace('Z','+00:00'))

class Upstream(BaseHTTPRequestHandler):
    def log_message(self,*_):pass
    def do_GET(self):self.reply({'object':'list','data':[{'id':'custom-model'}]})
    def reply(self,value):
        payload=json.dumps(value).encode();self.send_response(200);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(payload)));self.end_headers();self.wfile.write(payload)
    def do_POST(self):
        body=json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        with self.server.lock:self.server.posts+=1
        if body.get('stream'):
            self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
            self.wfile.write(start_frame('openai'));self.wfile.flush()
            # Close without terminal marker: real HTTP 200 stream failure.
        else:
            self.reply({'id':'test','object':'chat.completion','model':'custom-model','choices':[{'index':0,'message':{'role':'assistant','content':'hi'},'finish_reason':'stop'}],'usage':{'prompt_tokens':7,'completion_tokens':2,'prompt_tokens_details':{'cached_tokens':3,'cache_write_tokens':1}}})

def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--binary',required=True,type=Path);p.add_argument('--es-url',required=True);p.add_argument('--index-prefix',required=True);p.add_argument('--evidence',required=True,type=Path);p.add_argument('--docker',default='docker');a=p.parse_args()
    parsed=urlparse(a.es_url)
    if parsed.scheme!='http' or parsed.hostname not in ('127.0.0.1','localhost','::1') or parsed.username or parsed.path not in ('','/'):
        raise SystemExit('Smoke requires an explicitly supplied isolated loopback HTTP ES fixture, without credentials.')
    if not a.index_prefix.startswith('tidemux-smoke-') or any(c not in 'abcdefghijklmnopqrstuvwxyz0123456789-' for c in a.index_prefix):raise SystemExit('Use a fresh lowercase tidemux-smoke-* prefix.')
    root=a.evidence.resolve();root.mkdir(parents=True,exist_ok=False)
    def es(method,path,value=None):
        req=Request(a.es_url.rstrip('/')+path,None if value is None else json.dumps(value).encode(),{'Content-Type':'application/json'},method=method)
        with urlopen(req,timeout=10) as r:return json.load(r)
    version=es('GET','/')['version']['number']
    if version!='8.19.5':raise RuntimeError('Expected tested ES 8.19.5, got '+version)
    if es('GET','/_cat/indices/'+a.index_prefix+'*?format=json'):raise RuntimeError('Namespace already contains indices')
    template=json.loads((ROOT/'scripts/fixtures/elasticsearch/index-template.json').read_text());template['index_patterns']=[a.index_prefix+'*']
    es('PUT','/_index_template/'+a.index_prefix,template)
    with running_gateway(a.binary.resolve(),upstream_handler=Upstream) as gateway:
        status,_,headers=read_response(request(gateway,'openai',REQUEST_SENTINEL,session=SESSION_SENTINEL));success=headers['X-TideMux-Request-ID']
        if status!=200:raise RuntimeError('success fixture failed')
        status,data,headers=read_response(request(gateway,'openai',REQUEST_SENTINEL,stream=True,session=SESSION_SENTINEL));stream=headers['X-TideMux-Request-ID']
        if status!=200 or 'event: error' not in data:raise RuntimeError('missing HTTP 200 stream failure')
        from test_runtime_logs_package import request as raw_request
        status,_,headers=raw_request(gateway['url']+'/v1/unsupported','GET',token='synthetic-invalid');rejected=headers['X-TideMux-Request-ID']
        if status!=401:raise RuntimeError('local rejection fixture failed')
        gateway['process'].terminate();gateway['process'].wait(timeout=10)
        runtime=(gateway['root']/'stderr').read_text()
        events=[json.loads(line) for line in runtime.splitlines()]
    # Exercise the real binary's event path before config/Keychain reads.
    # These synthetic early failures cannot contact a provider or production ES.
    startup_failures=[]
    for options,stage,code in [(['--startup-private-argument'], 'arguments', 'invalid_arguments'),
                               (['--config',str(root/'startup-private-missing-config')], 'config', 'config_load_failed')]:
        failed=subprocess.run([str(a.binary.resolve()),'serve',*options],capture_output=True,text=True,timeout=10,
                              env={'PATH':'/usr/bin:/bin','HOME':str(root)})
        failure_events=[json.loads(line) for line in failed.stderr.splitlines() if line.strip()]
        if failed.returncode!=1 or len(failure_events)!=1:raise RuntimeError('missing unique early startup failure')
        failure=failure_events[0]
        if (failure.get('event'),failure.get('outcome'),failure.get('startup_stage'),failure.get('error_code'))!=('gateway_start','error',stage,code):raise RuntimeError('early startup classification mismatch')
        if 'startup-private' in failed.stderr or str(root) in failed.stderr:raise RuntimeError('startup input leaked')
        runtime+=failed.stderr;events+=failure_events;startup_failures.append((stage,code))
    binary_version=subprocess.check_output([str(a.binary.resolve()),'version'],text=True).strip()
    validate_runtime_events(events,binary_version,one_instance=False)
    if any(marker in runtime for marker in (GATEWAY_KEY,PROVIDER_KEY,REQUEST_SENTINEL,SESSION_SENTINEL)):raise RuntimeError('private data in runtime events')
    (root/'runtime.jsonl').write_text(runtime)
    now=datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace('+00:00','Z')
    synthetic={'timestamp':now,'level':'INFO','msg':'synthetic schema check','schema_version':2,'service':events[0]['service'],'event_id':'synthetic-schema-0001','event':'schema_smoke','requestId':'shared-smoke-id','outcome':'success','latency_ms':7,'queue_time_ms':2,'http_status':200,'upstream_attempted':True,'record_persisted':False}
    second=dict(synthetic,event='second_smoke',outcome='error',event_id='synthetic-schema-0002')
    bad=dict(synthetic,event='mapping_smoke',requestId='mapping-smoke-id',event_id='synthetic-mapping-bad',latency_ms='not-a-number')
    collector=root/'collector';collector.mkdir();collector.chmod(0o777)
    for name in ('data','dlq','dlq-reader'):(collector/name).mkdir();(collector/name).chmod(0o777)
    replay=runtime+'\n'.join(json.dumps(v) for v in (synthetic,second))+'\n'
    replay_ids={event['event_id'] for event in events}|{synthetic['event_id'],second['event_id']}
    (collector/'input.jsonl').write_text(replay+replay+json.dumps(bad)+'\nlegacy mixed non-JSON line\n')
    # Only transport security is removed for this isolated security-disabled fixture.
    conf=(ROOT/'scripts/fixtures/elasticsearch/tidemux.conf').read_text();conf='\n'.join(line for line in conf.splitlines() if not any(setting in line for setting in ('api_key =>','ssl_enabled =>','ssl_certificate_authorities =>')))+'\n'
    (collector/'pipeline.conf').write_text(conf);shutil.copy(ROOT/'scripts/fixtures/elasticsearch/logstash.yml',collector/'logstash.yml');shutil.copy(ROOT/'scripts/fixtures/elasticsearch/read-dlq.conf',collector/'read-dlq.conf')
    container_url='http://host.docker.internal:'+str(parsed.port or 9200)
    env={'TIDEMUX_LOG_PATH':'/work/input.jsonl','TIDEMUX_SINCEDB_PATH':'/work/sincedb','TIDEMUX_ES_INDEX_PREFIX':a.index_prefix,'TIDEMUX_ES_URL':container_url,'TIDEMUX_LOGSTASH_DATA':'/work/data','TIDEMUX_DLQ_PATH':'/work/dlq','TIDEMUX_PARSE_FAILURE_PATH':'/work/parse-failures.jsonl','TIDEMUX_MAPPING_FAILURE_PATH':'/work/mapping-failures.jsonl','LS_JAVA_OPTS':'-Xms256m -Xmx256m'}
    # Syntax-check the unmodified TLS/API-key reference with public CA material.
    (collector/'production.conf').write_text((ROOT/'scripts/fixtures/elasticsearch/tidemux.conf').read_text())
    certificate=ssl.create_default_context().get_ca_certs(binary_form=True)[0]
    (collector/'test-ca.pem').write_text(ssl.DER_cert_to_PEM_cert(certificate))
    name=a.index_prefix+'-collector' 
    def docker_run(settings, container=name):
        command=[a.docker,'run','--name',container,'-v',str(collector)+':/work']
        for key,value in settings.items():command+=['-e',key+'='+value]
        return command
    docker=docker_run(env)
    def run(command,filename,timeout=90):
        result=subprocess.run(command,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True,timeout=timeout);(root/filename).write_text(result.stdout)
        if result.returncode:raise RuntimeError(filename+' failed; inspect evidence')
    production=dict(env,TIDEMUX_ES_URL='https://es.example.invalid:9200',TIDEMUX_ES_API_KEY='synthetic-id:synthetic-key',TIDEMUX_ES_CA='/work/test-ca.pem')
    run(docker_run(production)+['--rm',IMAGE,'bin/logstash','--path.settings','/work','-f','/work/production.conf','--config.test_and_exit'],'production-config-check.log')
    run(docker+['--rm',IMAGE,'bin/logstash','--path.settings','/work','-f','/work/pipeline.conf','--config.test_and_exit'],'config-check.log')
    try:
        run(docker+['-d',IMAGE,'bin/logstash','--path.settings','/work','-f','/work/pipeline.conf'],'container-id.txt')
        deadline=time.monotonic()+90;documents=[]
        while time.monotonic()<deadline:
            indices=es('GET','/_cat/indices/'+a.index_prefix+'*?format=json')
            if indices:
                es('POST','/'+a.index_prefix+'*/_refresh')
                documents=es('POST','/'+a.index_prefix+'*/_search',{'size':100,'version':True,'query':{'match_all':{}}})['hits']['hits']
                if len(documents)==len(events)+3 and all(any(hit['_id']==identity and hit.get('_version')==2 for hit in documents) for identity in replay_ids) and (collector/'parse-failures.jsonl').exists() and list((collector/'dlq/main').glob('*.log')):break
            time.sleep(.5)
        else:raise RuntimeError('Indexing or failure capture timed out')
    finally:
        log=subprocess.run([a.docker,'logs',name],capture_output=True,text=True);(root/'collector.log').write_text(log.stdout+log.stderr)
        subprocess.run([a.docker,'stop','-t','15',name],capture_output=True,timeout=25)
        subprocess.run([a.docker,'rm',name],capture_output=True)
    sources=[hit['_source'] for hit in documents]
    def find(request_id,event,outcome,status):
        matches=[s for s in sources if s.get('tidemux',{}).get('requestId')==request_id and s['tidemux'].get('event')==event and s['tidemux'].get('outcome')==outcome and s['tidemux'].get('http_status')==status]
        if len(matches)!=1:raise RuntimeError('missing/duplicate request outcome '+str((event,outcome,status)))
        # Query exact typed fields, rather than trusting only _source.
        result=es('POST','/'+a.index_prefix+'*/_search',{'query':{'bool':{'filter':[{'term':{'tidemux.requestId':request_id}},{'term':{'event.action':event}},{'term':{'tidemux.outcome':outcome}},{'term':{'tidemux.http_status':status}}]}}})
        if result['hits']['total']['value']!=1:raise RuntimeError('typed search failed')
    find(success,'request_terminal','success',200);find(stream,'request_terminal','error',200);find(rejected,'local_rejection','rejected',401)
    for event in ('gateway_start','gateway_shutdown'):
        if not any(s.get('event',{}).get('action')==event for s in sources):raise RuntimeError('lifecycle missing')
    for stage,code in startup_failures:
        result=es('POST','/'+a.index_prefix+'*/_search',{'query':{'bool':{'filter':[{'term':{'event.action':'gateway_start'}},{'term':{'tidemux.outcome':'error'}},{'term':{'tidemux.startup_stage':stage}},{'term':{'tidemux.error_code':code}}]}}})
        if result['hits']['total']['value']!=1:raise RuntimeError('early startup typed query failed')
        source=result['hits']['hits'][0]['_source'];timestamp=event_time(source['@timestamp']);native=event_time(source['tidemux']['timestamp'])
        if abs(timestamp.timestamp()-native.timestamp())>.002:raise RuntimeError('early startup timestamp mismatch')
    for event in events:
        hit,=[hit for hit in documents if hit['_id']==event['event_id']]
        if hit['_source']['tidemux']!=event or hit['_version']!=2:raise RuntimeError('replay changed or duplicated a runtime event')
    shared=[s for s in sources if s.get('tidemux',{}).get('requestId')=='shared-smoke-id']
    if len(shared)!=2:raise RuntimeError('requestId overwrote distinct events')
    sample=next(s for s in shared if s['tidemux']['event']=='schema_smoke')
    if not isinstance(sample['tidemux']['latency_ms'],int) or sample['tidemux']['record_persisted'] is not False or sample['event']['action']!=sample['tidemux']['event']:raise RuntimeError('schema types or namespace wrong')
    timestamp=datetime.fromisoformat(sample['@timestamp'].replace('Z','+00:00'))
    if abs(timestamp.timestamp()-datetime.fromisoformat(now.replace('Z','+00:00')).timestamp())>.002:raise RuntimeError('timestamp mismatch')
    mapping=es('GET','/'+a.index_prefix+'*/_mapping');(root/'mapping.json').write_text(json.dumps(mapping,indent=2))
    for entry in mapping.values():
        properties=entry['mappings']['properties']['tidemux']['properties']
        for field,kind in [('requestId','keyword'),('event_id','keyword'),('event','keyword'),('startup_stage','keyword'),('latency_ms','long'),('http_status','long'),('record_persisted','boolean'),('timestamp','date')]:
            if properties[field]['type']!=kind:raise RuntimeError('ES mapping type mismatch')
    usage_query=es('POST','/'+a.index_prefix+'*/_search',{'size':0,'query':{'term':{'tidemux.requestId':success}},'aggs':{key:{'sum':{'field':'tidemux.message.usage.'+key}} for key in ('input_tokens','output_tokens','cache_read_input_tokens','cache_creation_input_tokens')}})
    for key,expected in [('input_tokens',3),('output_tokens',2),('cache_read_input_tokens',3),('cache_creation_input_tokens',1)]:
        if usage_query['aggregations'][key]['value']!=expected:raise RuntimeError('indexed usage aggregation mismatch: '+key)
    usage_model=es('POST','/'+a.index_prefix+'*/_search',{'query':{'bool':{'filter':[{'term':{'tidemux.requestId':success}},{'term':{'tidemux.message.model':'custom-model'}}]}}})
    if usage_model['hits']['total']['value']!=1:raise RuntimeError('canonical model keyword query failed')
    (root/'usage-aggregation.json').write_text(json.dumps(usage_query,indent=2))
    # A reader uses separate data and no DLQ writer, so it can run alongside main.
    reader=docker_run(dict(env,TIDEMUX_LOGSTASH_DATA='/work/dlq-reader'),name+'-dlq')
    settings=collector/'reader-settings';settings.mkdir();shutil.copy(ROOT/'scripts/fixtures/elasticsearch/read-dlq.yml',settings/'logstash.yml')
    reader_args=[IMAGE,'bin/logstash','--path.settings','/work/reader-settings','-f','/work/read-dlq.conf']
    run(reader+['--rm']+reader_args+['--config.test_and_exit'],'dlq-config-check.log')
    try:
        run(reader+['-d']+reader_args,'dlq-container-id.txt')
        deadline=time.monotonic()+60
        while time.monotonic()<deadline:
            failure=collector/'mapping-failures.jsonl'
            if failure.exists() and failure.stat().st_size:break
            time.sleep(.5)
        else:raise RuntimeError('DLQ reader did not expose mapping failure')
    finally:
        logs=subprocess.run([a.docker,'logs',name+'-dlq'],capture_output=True,text=True);(root/'dlq-reader.log').write_text(logs.stdout+logs.stderr)
        subprocess.run([a.docker,'stop','-t','10',name+'-dlq'],capture_output=True,timeout=20);subprocess.run([a.docker,'rm',name+'-dlq'],capture_output=True)
    failures=[json.loads(line) for line in (collector/'mapping-failures.jsonl').read_text().splitlines()]
    if not any(f.get('tidemux',{}).get('requestId')=='mapping-smoke-id' and 'latency_ms' in f['collector'].get('reason','') for f in failures):raise RuntimeError('mapping failure has no retained event/reason')
    (root/'documents.json').write_text(json.dumps(documents,indent=2))
    result={'binary':str(a.binary),'elasticsearch':version,'logstash':'8.19.5','index_prefix':a.index_prefix,'runtime_events':len(events),'indexed_documents':len(documents),'success_request_id':success,'stream_failure_request_id':stream,'rejection_request_id':rejected,'typed_queries':True,'canonical_usage_aggregations':True,'canonical_model_query':True,'startup_failure_queries':True,'ecs_namespace_and_timestamp':True,'distinct_same_request_events':True,'event_id_replayed_twice_deduplicated':True,'runtime_service_metadata_indexed':True,'parse_failure_retained':True,'mapping_failure_dlq_read':True,'privacy':True}
    (root/'result.json').write_text(json.dumps(result,indent=2)+'\n');print(json.dumps(result))

if __name__=='__main__':main()
