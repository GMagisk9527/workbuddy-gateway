using System.Text;

namespace HttpMigration;

// The native executable exposes the production CLI only. Offline probes and
// test entry points are validated by the JIT job and are not shipped in it.
internal static class AotProgram
{
    static Task Main(string[] args)
    {
        Console.InputEncoding=new UTF8Encoding(false);Console.OutputEncoding=new UTF8Encoding(false);
        if(args.Length is 4 or 5&&args[0] is "--standalone" or "--standalone-pool")return Program.RunStandalone(args);
        if (args is ["--state-check", var accounts, var keys])
        {
            DeploymentSupport.Check(accounts, keys);
            return Task.CompletedTask;
        }
        throw new ArgumentException("Use --standalone[-pool] <accounts> <keys> <port> [catalog] or --state-check <accounts> <keys>");
    }
}
