ALTER TABLE public.scheduled_jobs
    DROP COLUMN IF EXISTS local_time,
    DROP COLUMN IF EXISTS tz_name;
