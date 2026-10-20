# Optional dependency-only CPU runtime; source, data and results are mounted.
# No CUDA extras. Build and inspect its actual size before transferring it.
FROM python:3.11-slim-bookworm
COPY requirements.txt requirements.lock /tmp/
RUN pip install --no-cache-dir -r /tmp/requirements.txt -c /tmp/requirements.lock && rm /tmp/requirements.*
ENV CUDA_VISIBLE_DEVICES=-1 PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
WORKDIR /benchmark
