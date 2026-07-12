function check(a,b){if(a!==b)throw a+" !== "+b;} var s=0;loop:for(var i=0;i<5;i++){if(i===2)continue loop;s+=i;}check(s,8);console.log('PASS');
