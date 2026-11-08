namespace AccountScheduling;

// Pure eligibility/picker rules. The pool serializes lookup and permit reservation.
public static class SchedulingRules
{
    public static SelectionKind? Eligibility(AccountSnapshot a, string model, DateTimeOffset now, bool reservedProbe = false)
    {
        model = SchedulingCopy.Model(model);
        if (!a.CredentialsPresent || a.Disabled || a.CooldownUntil > now || !CanTrack(a, model, now)) return null;
        var state = a.Models?.GetValueOrDefault(model);
        if (state is null) return a.QuotaExhausted ? SelectionKind.ProbeExhausted : SelectionKind.Normal;
        if (state.CooldownUntil > now || state.QuotaBlocked) return null;
        if (!a.QuotaExhausted) return SelectionKind.Normal;
        if (state.Cost == ModelCost.Free) return SelectionKind.FreeExhausted;
        if (state.Cost == ModelCost.Paid || !reservedProbe && state.NextProbeAt > now) return null;
        return SelectionKind.ProbeExhausted;
    }
    internal static bool CanTrack(AccountSnapshot a, string model, DateTimeOffset now) => a.Models is null || a.Models.ContainsKey(model) || a.Models.Count < 1024 || a.Models.Values.Any(s => Inactive(s, now));
    internal static bool Inactive(AccountModelState s, DateTimeOffset now) => !s.QuotaBlocked && s.CooldownUntil <= now && s.NextProbeAt <= now;
    public static AccountChoice? Choose(IReadOnlyList<AccountSnapshot> accounts, string model, ModelPolicy policy,
        ModelPolicy global, IReadOnlyDictionary<AccountId, int> loads, int roundRobin, AccountId? preferred,
        IReadOnlySet<AccountId>? attempted, DateTimeOffset now, SchedulingOptions options)
    {
        model = SchedulingCopy.Model(model);
        if (accounts.Count == 0) return null;
        var preferredAccount = accounts.FirstOrDefault(a => a.Id == preferred);
        var free = policy.FreeSites?.ToHashSet() ?? [];
        foreach (var a in accounts)
            if (a.CredentialsPresent && a.HasAccessToken && !a.Disabled && a.Models?.GetValueOrDefault(model)?.Cost == ModelCost.Free) free.Add(a.Site);
        AccountChoice? Pick(bool freeOnly)
        {
            bool Allowed(AccountSnapshot a) => attempted?.Contains(a.Id) != true && (!freeOnly || free.Contains(a.Site)) &&
                (policy.Allow is not { Count: > 0 } || policy.Allow.Contains(a.Id)) && policy.Block?.Contains(a.Id) != true &&
                (global.Allow is not { Count: > 0 } || global.Allow.Contains(a.Id)) && global.Block?.Contains(a.Id) != true;
            foreach (var kind in new[] { SelectionKind.FreeExhausted, SelectionKind.Normal, SelectionKind.ProbeExhausted })
            {
                var candidates = accounts.Where(a => Allowed(a) && Eligibility(a, model, now) == kind).ToArray();
                if (candidates.Length == 0) continue;
                if (preferredAccount is not null && candidates.Contains(preferredAccount)) return new(preferredAccount, kind, false);
                int min = options.LoadAware ? candidates.Min(a => loads.GetValueOrDefault(a.Id)) : 0;
                bool AtLoad(AccountSnapshot a) => !options.LoadAware || loads.GetValueOrDefault(a.Id) == min;
                if (preferredAccount is null && options.ExpiringFirst)
                {
                    var expiring = candidates.Where(a => AtLoad(a) && a.ExpiringCredit > 0 && a.CreditExpiresAt > now && a.CreditExpiresAt <= now + options.Window)
                        .OrderBy(a => a.CreditExpiresAt).FirstOrDefault();
                    if (expiring is not null) return new(expiring, kind, true);
                }
                for (int i = 0; i < accounts.Count; i++)
                {
                    var a = accounts[((roundRobin % accounts.Count + accounts.Count) % accounts.Count + i) % accounts.Count];
                    if (AtLoad(a) && candidates.Contains(a)) return new(a, kind, false);
                }
            }
            return null;
        }
        if (free.Count > 0 && policy.PaidSites is { Count: > 0 } && Pick(true) is { } first) return first;
        return Pick(false);
    }
}
