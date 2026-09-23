-- 0058_studio_file_mod_time: 创作工作空间文件附注记下文件的修改时刻（mod_time）。
--   列目录对账时大小或修改时刻任一对不上即判「不经本包改过」，附注归零为 unknown；同大小的
--   改写也认得出。空串 = 还没记过（存量行），下一次列目录按当时的修改时刻补上，不归零。
ALTER TABLE studio_files ADD COLUMN mod_time TEXT NOT NULL DEFAULT '';
