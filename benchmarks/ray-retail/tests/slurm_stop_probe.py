import json,os,pathlib,signal,subprocess,sys,time
root=pathlib.Path(sys.argv[2])
def event(name,**kw):
    with (root/'events.jsonl').open('a') as f:
        f.write(json.dumps(dict(event=name,at=time.time(),pid=os.getpid(),pgid=os.getpgrp(),role=sys.argv[1],**kw))+'\n')
def wait_file(path,child,seconds=30):
    end=time.monotonic()+seconds
    while not path.exists() and child.poll() is None and time.monotonic()<end:
        time.sleep(.1)
    if not path.exists(): raise RuntimeError('Readiness missing: '+str(path))
role=sys.argv[1]
if role=='worker':
    mode=sys.argv[3]
    def end(*a):
        event('worker_term',mode=mode)
        sys.exit(0)
    signal.signal(signal.SIGTERM,end)
    (root/(mode+'-worker-ready')).touch()
    while True:time.sleep(.1)
elif role=='supervisor':
    mode=sys.argv[3]
    pending=[False]
    def request(*a):
        event('supervisor_term',mode=mode)
        pending[0]=True
    signal.signal(signal.SIGTERM,request)
    client=subprocess.Popen(['srun','--nodes=1','--ntasks=1','--cpus-per-task=1','--cpu-bind=none',
        sys.executable,__file__,'worker',str(root),mode])
    wait_file(root/(mode+'-worker-ready'),client)
    (root/(mode+'-supervisor-ready')).touch()
    while not pending[0]:time.sleep(.1)
    time.sleep(2)
    event('client_before_step_signal',mode=mode,exit_code=client.poll())
    steps=subprocess.check_output(['squeue','-s','-h','-j',os.environ['SLURM_JOB_ID'],'-o','%i'],universal_newlines=True).split()
    for step in steps:
        if step.split('.')[-1] not in ('batch','extern','interactive'):
            subprocess.check_call(['scancel','--signal=TERM',step])
    try:code=client.wait(timeout=15)
    except subprocess.TimeoutExpired:
        client.kill();code=client.wait()
    event('supervisor_done',mode=mode,client_exit=code)
else:
    assert os.environ['SLURM_JOB_NUM_NODES']=='10'
    for mode in ('inherited','isolated'):
        child=subprocess.Popen([sys.executable,__file__,'supervisor',str(root),mode],start_new_session=mode=='isolated')
        sent=[False]
        def relay(*a):
            event('batch_term',mode=mode)
            if not sent[0]:
                sent[0]=True
                child.send_signal(signal.SIGTERM)
        signal.signal(signal.SIGTERM,relay)
        wait_file(root/(mode+'-supervisor-ready'),child)
        subprocess.check_call(['scancel','--batch','--signal=TERM',os.environ['SLURM_JOB_ID']])
        child.wait(timeout=40)
        event('case_done',mode=mode,supervisor_exit=child.returncode)
    event('probe_complete')
