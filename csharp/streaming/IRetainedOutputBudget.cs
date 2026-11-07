namespace StreamingMigration;

public interface IRetainedOutputBudget
{
    void Add(long bytes);
    void Remove(long bytes);
    void Scratch(long bytes);
}
