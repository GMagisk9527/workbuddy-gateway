using System.Text;
using System.Text.Json;
using TypedMigration;

Console.InputEncoding = new UTF8Encoding(false); Console.OutputEncoding = new UTF8Encoding(false);
string? line;
while ((line = Console.ReadLine()) != null) {
    try {
        var request = WireReader.Read(Encoding.UTF8.GetBytes(line));
        // Input JsonDocument is already disposed here: domain owns open values.
        var output = WireWriter.Write(MessageMapper.Map(request));
        Console.Write("{\"ok\":true,\"result\":"); Console.Write(Encoding.UTF8.GetString(output)); Console.WriteLine("}");
    } catch (JsonException) { Console.WriteLine("{\"ok\":false,\"error\":\"invalid_json\"}"); }
}
