# LLM Model Configuration

This system supports multiple lightweight LLM models for better token usage and performance.

## Available Models

The system will automatically download and use these models:

1. **llama3.2:3b** (Default)
   - Size: ~2.0 GB
   - Good balance of quality and speed
   - Used by all services by default

2. **phi3:mini**
   - Size: ~2.3 GB
   - Very fast, small model
   - Good for quick responses

3. **qwen2.5:3b**
   - Size: ~2.0 GB
   - Good quality, efficient
   - Alternative to llama3.2

4. **gemma:2b**
   - Size: ~1.4 GB
   - Smallest, fastest model
   - Good for rapid responses

## Model Selection

### Automatic Selection

Each service tries models in order of preference:

- **Persona1**: llama3.2:3b → phi3:mini → qwen2.5:3b → gemma:2b
- **Persona2**: qwen2.5:3b → phi3:mini → llama3.2:3b → gemma:2b
- **Judge**: llama3.2:3b → qwen2.5:3b → phi3:mini → gemma:2b

### Manual Selection via Environment Variables

You can override the model selection:

```bash
# Use specific models for each service
export PERSONA1_MODEL="phi3:mini"
export PERSONA2_MODEL="qwen2.5:3b"
export JUDGE_MODEL="llama3.2:3b"

# Then start services
./bin/persona1
./bin/persona2
./bin/judge
```

## Benefits of Multiple Models

1. **Load Distribution**: Different models can run in parallel
2. **Token Optimization**: Smaller models use fewer tokens
3. **Performance**: Faster models for quick responses
4. **Flexibility**: Choose best model for each task

## Model Download

Models are automatically downloaded when Docker starts:

```bash
docker-compose up -d
```

The first startup will take longer as models download. Subsequent starts are faster.

## Storage Requirements

Total storage for all models: ~8-10 GB

- llama3.2:3b: ~2.0 GB
- phi3:mini: ~2.3 GB
- qwen2.5:3b: ~2.0 GB
- gemma:2b: ~1.4 GB

## Performance Comparison

| Model | Speed | Quality | Token Usage | Best For |
|-------|-------|---------|-------------|----------|
| gemma:2b | Fastest | Good | Lowest | Quick responses |
| phi3:mini | Very Fast | Good | Low | Fast debates |
| qwen2.5:3b | Fast | Very Good | Medium | Balanced |
| llama3.2:3b | Medium | Very Good | Medium | Default choice |

## Troubleshooting

If a model fails to load:

1. Check if model is downloaded:
   ```bash
   docker exec llmconvo-ollama-1 ollama list
   ```

2. Manually pull a model:
   ```bash
   docker exec llmconvo-ollama-1 ollama pull llama3.2:3b
   ```

3. Check service logs for model errors:
   ```bash
   tail -f logs/persona1.log
   ```

## Future Enhancements

- Model health checking
- Automatic fallback to alternative models
- Dynamic model selection based on load
- Per-round model switching for variety

