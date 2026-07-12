function check(a,b){if(a!==b)throw a+" !== "+b;} var f=0;outer:for(var i=0;i<3;i++){for(var j=0;j<3;j++){if(j===1){f++;break outer;}}}check(f,1);console.log('PASS');
