namespace KeySecurity;

// Implementations must not call back into KeyStore while holding their lock.
public interface IKeyAccounting { void Check(KeyView policy); }
