ALTER TABLE public.scheduled_jobs
    ADD COLUMN IF NOT EXISTS local_time text NULL,
    ADD COLUMN IF NOT EXISTS tz_name text NULL;

COMMENT ON COLUMN public.scheduled_jobs.local_time IS
    'Naive wall-clock time the job should fire at ("YYYY-MM-DDTHH:MM:SS"), in tz_name. '
    'NULL means the job has no wall-clock identity and fire_at (UTC) is authoritative '
    '(legacy rows and near-term one-shots).';
COMMENT ON COLUMN public.scheduled_jobs.tz_name IS
    'IANA timezone local_time is expressed in. NULL together with local_time means '
    'fire_at (UTC) is authoritative.';
