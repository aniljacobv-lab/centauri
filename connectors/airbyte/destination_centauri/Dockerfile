FROM python:3.11-slim
WORKDIR /airbyte
COPY main.py /airbyte/main.py
# Pure stdlib — nothing to pip install.
ENTRYPOINT ["python", "/airbyte/main.py"]
LABEL io.airbyte.name=airbyte/destination-centauri
LABEL io.airbyte.version=0.1.0
