package user

import (
	"dont/models"
	"dont/pkg/e"
	"dont/pkg/util"
	"fmt"
	"github.com/astaxie/beego/validation"
	"github.com/gin-gonic/gin"
	"log"
	"net/http"
)

type user struct {
	Username   string `form:"username" json:"username" uri:"username" xml:"username" binding:"required"`
	Password   string `form:"password" json:"password" uri:"password" xml:"password" binding:"required"`
	CaptchaId  string `form:"captchaId" json:"captchaId" uri:"captchaId" xml:"captchaId" binding:"required"`
	VerifyCode string `form:"verifyCode" json:"verifyCode" uri:"verifyCode" xml:"verifyCode" binding:"required"`
}
type userchange struct {
	Username  string `form:"username" json:"username" uri:"username" xml:"username" binding:"required"`
	Password1 string `form:"password1" json:"password1" uri:"password1" xml:"password1" binding:"required"`
	Password2 string `form:"password2" json:"password2" uri:"password2" xml:"password2" binding:"required"`
}

func Login(g *gin.Context) {
	var form user
	code := 200
	mess := "登陆成功"
	if g.Bind(&form) != nil {
		fmt.Print("空")
		if form.Username == "" || form.Password == "" {
			code = e.INVALID_PARAMS
			g.JSON(http.StatusOK, gin.H{
				"code": code,
				"msg":  e.GetMsg(code),
				"data": "用户名不存在或密码错误",
			})
		}
	} else {
		valid := validation.Validation{}
		a := user{Username: form.Username, Password: form.Password}
		ok, _ := valid.Valid(&a)
		data := make(map[string]interface{})
		code = e.INVALID_PARAMS
		if ok {
			isExist := models.CheckAuth(form.Username, form.Password)
			if isExist {
				token, err := util.GenerateToken(form.Username, form.Password)
				if err != nil {
					code = e.ERROR_AUTH_TOKEN
				} else {
					data["token"] = token
					code = e.SUCCESS
					mess = e.GetMsg(code)
				}
				g.SetCookie("login", "ok", 3600, "/", "", false, true)
				g.SetCookie("token", token, 3600, "/", "", false, true)
				models.Login_sent(form.Username)
			} else {
				code = e.ERROR_PASS
				mess = e.GetMsg(code)
			}
		} else {
			for _, err := range valid.Errors {
				log.Println(err.Key, err.Message)
			}
		}
		fmt.Println("验证码结果：", CaptVerify(form.CaptchaId, form.VerifyCode))
		fmt.Println(form.CaptchaId)
		fmt.Println(form.VerifyCode)
		if CaptVerify(form.CaptchaId, form.VerifyCode) {
			fmt.Println(form.CaptchaId, form.VerifyCode)
			fmt.Println("验证码正确")
		} else {
			code = e.INVALID_PARAMS
			mess = "验证码错误"
			delete(data, "token")
		}
		g.JSON(http.StatusOK, gin.H{
			"code":    code,
			"message": mess,
			"data":    data,
		})

	}

}
func ChangePass(g *gin.Context) {
	var form userchange
	code := 200
	if g.Bind(&form) != nil {
		fmt.Print("空")
		if form.Username == "" || form.Password1 == "" || form.Password2 == "" {
			code = e.INVALID_PARAMS
			g.JSON(http.StatusOK, gin.H{
				"code": code,
				"msg":  e.GetMsg(code),
				"data": "用户名不存在或密码错误",
			})
		}
	} else {
		isExist := models.CheckAuth(form.Username, form.Password1)
		if isExist {
			ischangeok := models.ChangePasswd(form.Username, form.Password1, form.Password2)
			if ischangeok {
				g.JSON(http.StatusOK, gin.H{
					"code": 200,
					"msg":  "密码修改成功",
					"data": "ok",
				})
			} else {
				g.JSON(http.StatusOK, gin.H{
					"code": e.ERROR_CHANGE,
					"msg":  e.GetMsg(e.ERROR_CHANGE),
					"data": "fail",
				})
			}
		} else {
			g.JSON(http.StatusOK, gin.H{
				"code": e.ERROR_PASS,
				"msg":  e.GetMsg(e.ERROR_PASS),
				"data": "fail",
			})
		}
	}

}
