FROM python:3.14-alpine

WORKDIR /app

COPY scripts/app.py /app/app.py

ENV PYTHONUNBUFFERED=1

EXPOSE 62225

STOPSIGNAL SIGTERM

CMD ["python", "/app/app.py"]
