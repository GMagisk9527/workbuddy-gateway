using System.Collections.Frozen;

namespace AccountScheduling;

public sealed class AccountLease : IDisposable
{
    Action? release;
    internal AccountPool Owner { get; }
    internal bool Active => Volatile.Read(ref release) is not null;
    public AccountSnapshot Account { get; }
    public SelectionKind Kind { get; }
    internal AccountLease(AccountChoice choice, AccountPool owner, Action release) { Account = choice.Account; Kind = choice.Kind; Owner = owner; this.release = release; }
    public void Dispose() => Interlocked.Exchange(ref release, null)?.Invoke();
}

public sealed partial class AccountPool
{
    public int Count { get { lock (sync) return accounts.Length; } }
    public SchedulingOptions Options{get{lock(sync)return options;}}
    readonly object sync = new();
    readonly TimeProvider time;
    AccountSnapshot[] accounts = [];
    FrozenDictionary<string, ModelPolicy> policies = FrozenDictionary<string, ModelPolicy>.Empty;
    ModelPolicy global = new();
    FrozenDictionary<string,ModelPolicy> catalogPolicies=FrozenDictionary<string,ModelPolicy>.Empty;
    SchedulingOptions options;
    readonly Dictionary<AccountId, int> loads = [];
    readonly Dictionary<AccountId,(bool Disabled,DateTimeOffset Cooldown)> admissionBaseline=[];
    readonly Dictionary<AccountId,DateTimeOffset> rateCooldowns=[];
    int active, waiting, roundRobin;
    TaskCompletionSource changed = NewSignal();
    static TaskCompletionSource NewSignal() => new(TaskCreationOptions.RunContinuationsAsynchronously);
    void Notify() { var old = changed; changed = NewSignal(); old.TrySetResult(); }
    public AccountPool(IEnumerable<AccountSnapshot> accounts, SchedulingOptions? options = null,
        IReadOnlyDictionary<string, ModelPolicy>? policies = null, ModelPolicy? global = null, TimeProvider? time = null)
    {
        this.options = options ?? new(); this.options.Validate(); this.time = time ?? TimeProvider.System;
        Replace(accounts, policies, global);
    }
    // Build complete immutable candidates before publishing; active permits survive reload/removal.
    public void Replace(IEnumerable<AccountSnapshot> input, IReadOnlyDictionary<string, ModelPolicy>? policies = null, ModelPolicy? global = null)
    {
        var next = input.Take(1025).Select(SchedulingCopy.Account).ToArray();
        if (next.Length > 1024 || next.Select(a => a.Id).Distinct().Count() != next.Length) throw new ArgumentException("Invalid account pool");
        if (policies is { Count: > 1024 }) throw new ArgumentException("Too many scheduling policies");
        var nextPolicies = (policies ?? new Dictionary<string, ModelPolicy>()).ToFrozenDictionary(p => SchedulingCopy.Model(p.Key), p => SchedulingCopy.Policy(p.Value));
        var nextGlobal = SchedulingCopy.Policy(global ?? new());
        lock (sync)
        {
            var now = time.GetUtcNow();
            for (int i = 0; i < next.Length; i++)
            {
                var old = accounts.FirstOrDefault(a => a.Id == next[i].Id);
                if (old?.Models is null) continue;
                var states = new Dictionary<string, AccountModelState>(next[i].Models!);
                foreach (var (model, state) in old.Models)
                {
                    if (state.NextProbeAt <= now) continue;
                    if (!states.ContainsKey(model) && states.Count == 1024)
                    {
                        var candidate = states.Where(p => SchedulingRules.Inactive(p.Value, now)).OrderBy(p => p.Value.ObservedAt).ThenBy(p => p.Key, StringComparer.Ordinal).FirstOrDefault();
                        if (candidate.Key is null) throw new ArgumentException("Reload cannot discard a live probe reservation");
                        states.Remove(candidate.Key);
                    }
                    var incoming = states.GetValueOrDefault(model) ?? new();
                    if (incoming.NextProbeAt < state.NextProbeAt) states[model] = incoming with { NextProbeAt = state.NextProbeAt };
                }
                next[i] = next[i] with { Models = states.ToFrozenDictionary() };
            }
            admissionBaseline.Clear();foreach(var a in next)admissionBaseline[a.Id]=(a.Disabled,a.CooldownUntil);
            foreach(var id in rateCooldowns.Keys.Where(id=>!next.Any(a=>a.Id==id)).ToArray())rateCooldowns.Remove(id);
            for(int i=0;i<next.Length;i++)if(rateCooldowns.GetValueOrDefault(next[i].Id)>next[i].CooldownUntil)next[i]=next[i] with{CooldownUntil=rateCooldowns[next[i].Id]};
            if(runtimeInitialized)foreach(var a in next)runtimeTouched.Add(a.Id);
            runtimeTouched.RemoveWhere(id=>!next.Any(a=>a.Id==id));runtimeRestored.RemoveWhere(id=>!next.Any(a=>a.Id==id));runtimeInitialized=true;runtimeVersion++;
            accounts = next; this.policies = nextPolicies; this.global = nextGlobal; roundRobin %= Math.Max(1, next.Length); Notify();
        }
    }
    public void Configure(SchedulingOptions next) { next.Validate(); lock (sync) { options = next; Notify(); } }
    public void SetCatalogPolicies(IReadOnlyDictionary<string,ModelPolicy> next)
    {var copy=next.ToFrozenDictionary(p=>SchedulingCopy.Model(p.Key),p=>SchedulingCopy.Policy(p.Value));lock(sync){catalogPolicies=copy;Notify();}}
    ModelPolicy Policy(string model)
    {
        var configured=policies.GetValueOrDefault(model)??new();var catalog=catalogPolicies.GetValueOrDefault(model);
        return catalog is null?configured:configured with{FreeSites=(configured.FreeSites??new HashSet<AccountSite>()).Concat(catalog.FreeSites??new HashSet<AccountSite>()).ToHashSet(),PaidSites=(configured.PaidSites??new HashSet<AccountSite>()).Concat(catalog.PaidSites??new HashSet<AccountSite>()).ToHashSet()};
    }
    public bool Allows(AccountId id,string model)
    {lock(sync){var rule=Policy(SchedulingCopy.Model(model));return accounts.Any(a=>a.Id==id&&!a.Disabled&&a.CredentialsPresent)&&
        (rule.Allow is not{Count:>0}||rule.Allow.Contains(id))&&rule.Block?.Contains(id)!=true&&
        (global.Allow is not{Count:>0}||global.Allow.Contains(id))&&global.Block?.Contains(id)!=true;}}
    public bool SchedulingEnabled(AccountId id)
    {lock(sync)return (global.Allow is not{Count:>0}||global.Allow.Contains(id))&&global.Block?.Contains(id)!=true;}
    public void SetAccountPolicies(IReadOnlyDictionary<string,ModelPolicy> next,ModelPolicy all,SchedulingOptions? limits=null)
    {
        var copy=next.ToFrozenDictionary(p=>SchedulingCopy.Model(p.Key),p=>SchedulingCopy.Policy(p.Value));var globalCopy=SchedulingCopy.Policy(all);limits?.Validate();
        lock(sync){policies=copy;global=globalCopy;if(limits is not null)options=limits;Notify();}
    }
    public void ReconcileAccounts(IEnumerable<AccountSnapshot> configured,IReadOnlySet<AccountId> changedCredentials,Action publishAvailability)
    {
        lock(sync)
        {
            var next=configured.Select(a=>accounts.FirstOrDefault(old=>old.Id==a.Id) is{} old
                ?old with{Disabled=a.Disabled,CredentialsPresent=true,HasAccessToken=true,CooldownUntil=default,
                    CreditExpiresAt=changedCredentials.Contains(a.Id)?default:old.CreditExpiresAt,ExpiringCredit=changedCredentials.Contains(a.Id)?0:old.ExpiringCredit}
                :a).ToArray();
            Replace(next,policies,global);publishAvailability();
        }
    }
    public void EnableExpiringPriority(){lock(sync){options=options with{ExpiringFirst=true};Notify();}}
    public bool HasEligibleAccount(string model,IReadOnlySet<AccountId> excluded)
    {lock(sync)return SchedulingRules.Choose(accounts,model,Policy(model),global,loads,roundRobin,null,excluded,time.GetUtcNow(),options) is not null;}
    public void ReplaceCredentialAccount(AccountSnapshot replacement,bool sameIdentity,Action publishAvailability)
    {
        lock(sync)
        {
            int index=Array.FindIndex(accounts,a=>a.Id==replacement.Id);if(index<0)throw new ArgumentException("Account no longer configured");
            var old=accounts[index];
            accounts[index]=sameIdentity?old with{Upstream=replacement.Upstream,Site=replacement.Site,Disabled=replacement.Disabled,CredentialsPresent=true,HasAccessToken=true}:SchedulingCopy.Account(replacement);
            if(!sameIdentity){rateCooldowns.Remove(replacement.Id);runtimeRestored.Remove(replacement.Id);}
            admissionBaseline[replacement.Id]=(replacement.Disabled,default);MarkRuntime(replacement.Id);publishAvailability();Notify();
        }
    }
    public void SetCredentialAvailability(AccountId id, bool present, bool disabled, DateTimeOffset cooldownUntil)
    {
        lock (sync)
        {
            int i = Array.FindIndex(accounts, a => a.Id == id); if (i < 0) return;
            var a = accounts[i];
            accounts[i] = a with
            {
                CredentialsPresent = present,
                HasAccessToken = present,
                Disabled = a.Disabled || disabled,
                CooldownUntil = a.CooldownUntil > cooldownUntil ? a.CooldownUntil : cooldownUntil
            };
            Notify();
        }
    }
    public void RestoreCredentialAvailability(AccountId id,bool present)
    {
        lock(sync)
        {
            int i=Array.FindIndex(accounts,a=>a.Id==id);if(i<0)return;
            var baseline=admissionBaseline[id];var rate=rateCooldowns.GetValueOrDefault(id);accounts[i]=accounts[i] with{CredentialsPresent=present,HasAccessToken=present,Disabled=baseline.Disabled,CooldownUntil=rate>baseline.Cooldown?rate:baseline.Cooldown};Notify();
        }
    }
    public bool IsConfiguredDisabled(AccountId id){lock(sync)return admissionBaseline.TryGetValue(id,out var baseline)&&baseline.Disabled;}
    public void ObserveQuota(AccountId id,bool exhausted,DateTimeOffset creditExpiresAt,double expiringCredit)
    {
        if(!double.IsFinite(expiringCredit)||expiringCredit<0)throw new ArgumentException("Invalid credit observation");
        lock(sync)
        {
            int i=Array.FindIndex(accounts,a=>a.Id==id);if(i<0)return;var old=accounts[i];var models=old.Models;
            if(!exhausted&&models is not null)models=models.ToFrozenDictionary(p=>p.Key,p=>p.Value with{QuotaBlocked=false,NextProbeAt=default});
            accounts[i]=old with{QuotaExhausted=exhausted,CreditExpiresAt=creditExpiresAt,ExpiringCredit=expiringCredit,Models=models};MarkRuntime(id);Notify();
        }
    }
    public void ClearCreditPriority(AccountId id)
    {lock(sync){int i=Array.FindIndex(accounts,a=>a.Id==id);if(i<0)return;accounts[i]=accounts[i] with{CreditExpiresAt=default,ExpiringCredit=0};Notify();}}
    public void RestoreQuotaBlock(AccountId id)
    {lock(sync){int i=Array.FindIndex(accounts,a=>a.Id==id);if(i<0)return;accounts[i]=accounts[i] with{QuotaExhausted=true};runtimeVersion++;Notify();}}
    public AccountSnapshot? Snapshot(AccountId id){lock(sync)return accounts.FirstOrDefault(a=>a.Id==id);}
    public AccountSnapshot[] Snapshots(){lock(sync)return accounts.ToArray();}
    public ModelRuntimeStatus? ModelStatus(AccountId id)
    {
        lock(sync)
        {
            var a=accounts.FirstOrDefault(a=>a.Id==id);if(a is null)return null;var now=time.GetUtcNow();var states=a.Models!.Values;
            return new(a.Models.Count,states.Count(s=>s.Cost==ModelCost.Free),states.Count(s=>s.Cost==ModelCost.Paid),states.Count(s=>s.CooldownUntil>now),states.Count(s=>s.QuotaBlocked),a.CooldownUntil);
        }
    }
    public void ObserveModelUsage(AccountId id,string model,string? raw)
    {
        var cost=ModelObservations.Cost(raw);if(cost==ModelCost.Unknown)return;
        UpdateModel(id,model,(state,now)=>state with{Cost=cost,QuotaBlocked=cost==ModelCost.Free?false:state.QuotaBlocked,ObservedAt=now});
    }
    public void ObserveModelFailure(AccountId id,string model,int status,string? code,string? raw=null)
    {
        model=SchedulingCopy.Model(model);
        lock(sync)
        {
            var observation=ModelObservations.Failure(status,code,raw,time.GetUtcNow());
            if(observation.Kind==ModelRejection.AccountRate)
            {
                int i=Array.FindIndex(accounts,a=>a.Id==id);if(i<0)return;
                var rate=observation.Until>rateCooldowns.GetValueOrDefault(id)?observation.Until:rateCooldowns[id];
                var end=rate>accounts[i].CooldownUntil?rate:accounts[i].CooldownUntil;
                rateCooldowns[id]=rate;accounts[i]=accounts[i] with{CooldownUntil=end};MarkRuntime(id);Notify();
            }
            else if(observation.Kind==ModelRejection.ModelRate)UpdateModel(id,model,(s,_)=>s with{CooldownUntil=observation.Until>s.CooldownUntil?observation.Until:s.CooldownUntil});
            else if(observation.Kind==ModelRejection.Quota)UpdateModel(id,model,(s,now)=>s with{QuotaBlocked=true,ObservedAt=now});
        }
    }
    void UpdateModel(AccountId id,string model,Func<AccountModelState,DateTimeOffset,AccountModelState> update)
    {
        model=SchedulingCopy.Model(model);
        lock(sync)
        {
            int i=Array.FindIndex(accounts,a=>a.Id==id);if(i<0)return;
            var a=accounts[i];var now=time.GetUtcNow();var states=new Dictionary<string,AccountModelState>(a.Models!);
            if(!states.ContainsKey(model)&&states.Count==1024)
            {
                var evict=states.Where(p=>SchedulingRules.Inactive(p.Value,now)).OrderBy(p=>p.Value.ObservedAt).ThenBy(p=>p.Key,StringComparer.Ordinal).FirstOrDefault();
                if(evict.Key is null)return;states.Remove(evict.Key);
            }
            states[model]=update(states.GetValueOrDefault(model)??new(),now);accounts[i]=a with{Models=states.ToFrozenDictionary()};MarkRuntime(id);Notify();
        }
    }
    // Recheck after waiting for credential maintenance, before WAL begin or network send.
    public void Revalidate(AccountLease lease, string model)
    {
        model = SchedulingCopy.Model(model);
        lock (sync)
        {
            if (lease.Owner != this || !lease.Active) throw new SchedulingFailure("account_lease_invalid");
            var current = accounts.FirstOrDefault(a => a.Id == lease.Account.Id);
            bool Allowed(ModelPolicy p) => current is not null && (p.Allow is not { Count: > 0 } || p.Allow.Contains(current.Id)) && p.Block?.Contains(current.Id) != true;
            if (current is null || current.Upstream != lease.Account.Upstream || current.Site != lease.Account.Site ||
                !Allowed(policies.GetValueOrDefault(model) ?? new()) || !Allowed(global)) throw new SchedulingFailure("account_changed_while_waiting");
            var kind = SchedulingRules.Eligibility(current, model, time.GetUtcNow(), lease.Kind == SelectionKind.ProbeExhausted);
            if (kind is null || kind == SelectionKind.ProbeExhausted && lease.Kind != SelectionKind.ProbeExhausted) throw new SchedulingFailure("account_changed_while_waiting");
        }
    }
    public SchedulingStatus Status { get { lock (sync) return new(active, waiting, loads.ToFrozenDictionary()); } }
    public Task<AccountLease> AcquireAsync(string model, AccountId? preferred = null, IReadOnlySet<AccountId>? attempted = null, CancellationToken cancellation = default)
        => AcquireCoreAsync(model,preferred,attempted,cancellation,false);
    // Cache hints are untrusted/stale. Filter them under the same scheduling lock
    // on every wake-up, preserving the existing selector for all other callers.
    public Task<AccountLease> AcquireAffinityAsync(string model, AccountId? preferred = null, IReadOnlySet<AccountId>? attempted = null, CancellationToken cancellation = default,TimeSpan? cacheGrace=null)
        => AcquireCoreAsync(model,preferred,attempted,cancellation,true,cacheGrace);
    async Task<AccountLease> AcquireCoreAsync(string model, AccountId? preferred, IReadOnlySet<AccountId>? attempted, CancellationToken cancellation, bool validateAffinity,TimeSpan? cacheGrace=null)
    {
        model = SchedulingCopy.Model(model);
        var excluded = attempted?.ToFrozenSet();
        SchedulingOptions initial; lock (sync) initial = options;
        if(cacheGrace is{} extra&&extra>initial.Grace)initial=initial with{AffinityWait=extra<initial.CacheGrace?extra:initial.CacheGrace};
        long started = time.GetTimestamp();
        bool queued = false;
        try
        {
            while (true)
            {
                cancellation.ThrowIfCancellationRequested();
                Task signal; TimeSpan delay;
                lock (sync)
                {
                    // Cancellation must win over an available slot, including a race after wake-up.
                    cancellation.ThrowIfCancellationRequested();
                    var elapsed = time.GetElapsedTime(started);
                    if (elapsed >= initial.Wait) throw new SchedulingFailure("scheduling_timeout");
                    var prefer = !initial.LoadAware || elapsed < initial.Grace ? preferred : null;
                    var now = time.GetUtcNow();
                    if(validateAffinity && prefer is not null)
                    {
                        var eligible=SchedulingRules.Choose(accounts,model,Policy(model),global,
                            loads,roundRobin,prefer,excluded,now,options with{ExpiringFirst=false});
                        if(eligible?.Account.Id!=prefer)prefer=null;
                    }
                    var choice = SchedulingRules.Choose(accounts, model, Policy(model), global,
                        loads, roundRobin, prefer, excluded, now, options);
                    if (choice is null) throw new SchedulingFailure("no_eligible_account");
                    var a = choice.Account;
                    if (loads.GetValueOrDefault(a.Id) < options.MaxAccount && (options.MaxGlobal == 0 || active < options.MaxGlobal))
                    {
                        if (choice.Kind == SelectionKind.ProbeExhausted)
                        {
                            var states = new Dictionary<string, AccountModelState>(a.Models!);
                            if (!states.ContainsKey(model) && states.Count == 1024)
                            {
                                string evict = states.Where(p => SchedulingRules.Inactive(p.Value, now)).OrderBy(p => p.Value.ObservedAt).ThenBy(p => p.Key, StringComparer.Ordinal).First().Key;
                                states.Remove(evict);
                            }
                            states[model] = (states.GetValueOrDefault(model) ?? new()) with { NextProbeAt = now + TimeSpan.FromMinutes(5) };
                            a = a with { Models = states.ToFrozenDictionary() };
                            accounts[Array.FindIndex(accounts, x => x.Id == a.Id)] = a;
                            MarkRuntime(a.Id);
                            choice = choice with { Account = a };
                        }
                        roundRobin = (Array.FindIndex(accounts, x => x.Id == a.Id) + 1) % accounts.Length;
                        active++; loads[a.Id] = loads.GetValueOrDefault(a.Id) + 1;
                        return new(choice, this, () => Release(a.Id));
                    }
                    if (!queued)
                    {
                        if (waiting >= options.MaxQueued) throw new SchedulingFailure("scheduling_queue_full");
                        waiting++; queued = true;
                    }
                    signal = changed.Task;
                    delay = TimeSpan.FromMilliseconds(100);
                    delay = delay < initial.Wait - elapsed ? delay : initial.Wait - elapsed;
                    if (prefer is not null && initial.LoadAware && initial.Grace - elapsed > TimeSpan.Zero && initial.Grace - elapsed < delay) delay = initial.Grace - elapsed;
                }
                try { await signal.WaitAsync(delay, time, cancellation); } catch (TimeoutException) { }
            }
        }
        finally { if (queued) lock (sync) waiting--; }
    }
    void Release(AccountId id)
    {
        lock (sync) { active--; if (--loads[id] == 0) loads.Remove(id); Notify(); }
    }
}
