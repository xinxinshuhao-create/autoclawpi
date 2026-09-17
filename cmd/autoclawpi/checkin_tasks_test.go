package main

import (
	"testing"
)

// TestCheckinTasksAreDailyOnly 确认每日签到列表只含真正的每日任务。
//
// 背景：newbie_* 是「一次性」奖励（一次领完，之后永远是 already）。
// 把它们放在每日 checkin 里会让每次签到都白跑一趟上游 ——
// 既浪费请求，又增加「行为规律性」（封号主因之一）。
// 它们改在加号时领取（claimNewbieRewards）。
func TestCheckinTasksAreDailyOnly(t *testing.T) {
	if len(checkinTasks) == 0 {
		t.Fatal("checkinTasks 不能为空")
	}
	for _, task := range checkinTasks {
		if task.ID == "" {
			t.Error("任务 ID 不能为空")
		}
		if len(task.ID) >= 7 && task.ID[:7] == "newbie_" {
			t.Errorf("每日签到不应包含一次性任务: %s", task.ID)
		}
	}
	// 每日任务应含 daily_signin
	found := false
	for _, task := range checkinTasks {
		if task.ID == "daily_signin" {
			found = true
		}
	}
	if !found {
		t.Error("每日签到必须包含 daily_signin")
	}
}

// TestNewbieTasksAreOneTime 确认新手任务列表内容正确且与每日列表不重叠。
func TestNewbieTasksAreOneTime(t *testing.T) {
	if len(newbieTasks) == 0 {
		t.Fatal("newbieTasks 不能为空")
	}
	daily := map[string]bool{}
	for _, task := range checkinTasks {
		daily[task.ID] = true
	}
	for _, task := range newbieTasks {
		if daily[task.ID] {
			t.Errorf("任务 %s 同时出现在每日与一次性列表", task.ID)
		}
		if len(task.ID) < 7 || task.ID[:7] != "newbie_" {
			t.Errorf("newbieTasks 应只含 newbie_* 任务，实际: %s", task.ID)
		}
	}
	// 已知的两个新手任务
	want := map[string]bool{"newbie_cloud_lobster": false, "newbie_local_lobster": false}
	for _, task := range newbieTasks {
		if _, ok := want[task.ID]; ok {
			want[task.ID] = true
		}
	}
	for id, seen := range want {
		if !seen {
			t.Errorf("缺少预期的新手任务: %s", id)
		}
	}
}
