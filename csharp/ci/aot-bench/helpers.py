import http.client
import json
import os
import pathlib
import subprocess
import time


KEY = "synthetic-pressure-key"


def proc(pid):
    try:
        lines = dict(x.split(':', 1) for x in pathlib.Path(f'/proc/{pid}/status').read_text().splitlines() if ':' in x)
        stat = pathlib.Path(f'/proc/{pid}/stat').read_text().rsplit(')', 1)[1].split()
        return {'rss': int(lines['VmRSS'].split()[0]) * 1024, 'hwm': int(lines['VmHWM'].split()[0]) * 1024,
                'cpu_s': (int(stat[11]) + int(stat[12])) / os.sysconf('SC_CLK_TCK')}
    except (OSError, KeyError):
        return {'rss': 0, 'hwm': 0, 'cpu_s': 0}


def available():
    return next(int(x.split()[1]) * 1024 for x in pathlib.Path('/proc/meminfo').read_text().splitlines() if x.startswith('MemAvailable:'))


def make_body(protocol, size):
    # Includes tool history, Unicode, escapes and a small tool catalogue.
    line = 'synthetic source line: 中文 <tag> "quoted" \\ path\n'
    text = (line * (size // len(line.encode()) + 1)).encode()[:size].decode('utf-8', errors='ignore')
    tools = [{'name': 'read_file', 'description': 'Read synthetic source', 'input_schema': {'type': 'object', 'properties': {'path': {'type': 'string'}}}}]
    for i in range(7):
        tools.append({'name': f'tool_{i}', 'description': 'synthetic', 'input_schema': {'type': 'object'}})
    if protocol == 'messages':
        body = {'model': 'bench-model', 'stream': True, 'max_tokens': 512, 'system': 'You are WorkBuddy.', 'tools': tools,
                'messages': [{'role': 'user', 'content': 'Read the file.'}, {'role': 'assistant', 'content': [{'type': 'tool_use', 'id': 'synthetic-call', 'name': 'read_file', 'input': {'path': 'source.txt'}}]}, {'role': 'user', 'content': [{'type': 'tool_result', 'tool_use_id': 'synthetic-call', 'content': text}]}]}
    elif protocol == 'chat':
        body = {'model': 'bench-model', 'stream': True, 'max_tokens': 512,
                'tools': [{'type': 'function', 'function': {'name': x['name'], 'description': x['description'], 'parameters': x['input_schema']}} for x in tools],
                'messages': [{'role': 'system', 'content': 'You are WorkBuddy.'}, {'role': 'user', 'content': 'Read the file.'}, {'role': 'assistant', 'tool_calls': [{'id': 'synthetic-call', 'type': 'function', 'function': {'name': 'read_file', 'arguments': '{"path":"source.txt"}'}}]}, {'role': 'tool', 'tool_call_id': 'synthetic-call', 'content': text}]}
    else:
        body = {'model': 'bench-model', 'stream': True, 'max_output_tokens': 512, 'instructions': 'You are WorkBuddy.',
                'tools': [{'type': 'function', 'name': x['name'], 'description': x['description'], 'parameters': x['input_schema']} for x in tools],
                'input': [{'role': 'user', 'content': 'Read the file.'}, {'type': 'function_call', 'call_id': 'synthetic-call', 'name': 'read_file', 'arguments': '{"path":"source.txt"}'}, {'type': 'function_call_output', 'call_id': 'synthetic-call', 'output': text}]}
    return json.dumps(body, ensure_ascii=False, separators=(',', ':')).encode()


def request(port, protocol, body, chunks, encoding='identity', barrier=None):
    if barrier:
        barrier.wait(timeout=10)
    started = time.perf_counter(); first = None; count = 0; terminal = False; failure = None
    connection = http.client.HTTPConnection('127.0.0.1', port, timeout=20)
    headers = {'Authorization': 'Bearer ' + KEY, 'Content-Type': 'application/json'}
    if encoding != 'identity':
        headers['Content-Encoding'] = encoding
    path = {'messages': '/v1/messages', 'chat': '/v1/chat/completions', 'responses': '/v1/responses'}[protocol]
    try:
        connection.request('POST', path, body, headers)
        response = connection.getresponse()
        if response.status != 200:
            raw = response.read(16384)
            return {'status': response.status, 'ok': False, 'error': raw.decode(errors='replace')[:200], 'total_ms': (time.perf_counter()-started)*1000}
        for line in response:
            if not line.startswith(b'data:'):
                continue
            data = line[5:].strip()
            if data == b'[DONE]':
                if protocol != 'messages': terminal = True
                continue
            if not data:
                continue
            event = json.loads(data)
            text = ''
            if protocol == 'messages':
                if event.get('type') == 'content_block_delta': text = event.get('delta', {}).get('text', '')
                if event.get('type') == 'message_stop': terminal = True
            elif protocol == 'chat':
                for choice in event.get('choices', []): text += choice.get('delta', {}).get('content') or ''
            elif event.get('type') == 'response.output_text.delta':
                text = event.get('delta', '')
            if event.get('type') in ('error', 'response.failed') or event.get('error'):
                failure = 'error event'
            if text:
                if first is None: first = (time.perf_counter() - started) * 1000
                count += len(text)
        return {'status': 200, 'ok': terminal and count == 32 * chunks and not failure, 'ttft_ms': first,
                'total_ms': (time.perf_counter()-started)*1000, 'output_chars': count, 'error': failure}
    except Exception as exc:
        return {'status': 0, 'ok': False, 'error': type(exc).__name__ + ': ' + str(exc)[:140], 'total_ms': (time.perf_counter()-started)*1000}
    finally:
        connection.close()


def percentile(values, fraction):
    if not values: return None
    ordered = sorted(values); position = (len(ordered)-1)*fraction; index = int(position)
    return round(ordered[index] + (ordered[min(index+1,len(ordered)-1)]-ordered[index])*(position-index), 3)


def wait_ready(process, port):
    for _ in range(100):
        if process.poll() is not None: raise RuntimeError(f'Child exited: {process.returncode}')
        try:
            c = http.client.HTTPConnection('127.0.0.1',port,timeout=1); c.request('GET','/readyz'); r=c.getresponse(); r.read(); c.close()
            if r.status == 200: return
        except OSError:
            pass
        time.sleep(.05)
    raise RuntimeError('Benchmark readiness timed out')


def stop(process):
    if process and process.poll() is None:
        process.terminate()
        try: process.wait(timeout=3)
        except subprocess.TimeoutExpired: process.kill(); process.wait(timeout=3)
